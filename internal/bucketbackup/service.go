// Package bucketbackup snapshots a managed S3 bucket into backup storage and
// restores a snapshot into a new, empty bucket.
//
// A snapshot is a directory <prefix>/<server>/<bucket>/<timestamp>/ holding
// every object under objects/<key> and, written last, manifest.json listing
// each object's key, size and sha256. A snapshot without a manifest is
// incomplete and never offered for restore. Objects written while a snapshot
// runs may or may not be in it: a snapshot is a copy taken over time, not a
// point-in-time image.
package bucketbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/backup"
)

var (
	// ErrRunning: a backup or restore of this configuration is already running.
	ErrRunning = errors.New("a backup or restore of this bucket is already running")
	// ErrNoSnapshot: the snapshot does not exist or is incomplete.
	ErrNoSnapshot = errors.New("no complete snapshot with that timestamp")
	// ErrTargetNotEmpty: a restore only ever goes into an empty bucket.
	ErrTargetNotEmpty = errors.New("the restore target bucket is not empty")
)

const manifestName = "manifest.json"

// tsLayout names a snapshot directory; tsRe validates one coming from a form.
const tsLayout = "2006-01-02T15-04-05Z"

var tsRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z$`)

// ErrUnsafeKey: an object key that, as part of an mc path, could leave its
// bucket ("..", empty or "." segments, a leading slash, a backslash).
var ErrUnsafeKey = errors.New("object key is not a plain path inside the bucket")

// SafeKey reports whether key stays inside its bucket when joined into an mc
// path. Keys come back from the backup storage on a restore, which anyone
// holding that storage's credentials could edit, and mc runs with the
// server's root credentials.
func SafeKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Target is a bucket on a managed S3 server, with the server's root
// credentials that read and write it.
type Target struct {
	Server   string // the server's service name (its app_name)
	Bucket   string
	RootUser string
	RootPass string
}

// Entry is one object in a bucket.
type Entry struct {
	Key  string
	Size int64
}

// BucketIO reads and writes the objects of a bucket on a managed S3 server.
type BucketIO interface {
	List(ctx context.Context, t Target) ([]Entry, error)
	Read(ctx context.Context, t Target, key string, w io.Writer) error
	Write(ctx context.Context, t Target, key string, r io.Reader) error
}

// Row is a backup configuration.
type Row struct {
	ID            int64
	BucketID      int64
	DestinationID int64
	Prefix        string
	Retention     int
}

// Store is what the service reads and records.
type Store interface {
	GetBackup(ctx context.Context, id int64) (Row, error)
	GetTarget(ctx context.Context, bucketID int64) (Target, error)
	GetDestination(ctx context.Context, id int64) (backup.Destination, error)
	SetBackupResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error
	SetRestoreResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error
}

// Notifier is the optional failure sink (implemented by *notify.Service).
type Notifier interface {
	BackupFailed(ctx context.Context, backupID int64, reason string)
}

// ManifestObject is one object of a snapshot.
type ManifestObject struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes a complete snapshot.
type Manifest struct {
	Version int              `json:"version"`
	Server  string           `json:"server"`
	Bucket  string           `json:"bucket"`
	Taken   time.Time        `json:"taken"`
	Objects []ManifestObject `json:"objects"`
}

// Snapshot is one snapshot directory in backup storage.
type Snapshot struct {
	Timestamp string
	Complete  bool
	Objects   int
	Bytes     int64
}

// Service runs bucket backups and restores.
type Service struct {
	io           BucketIO
	store        Store
	notifier     Notifier
	allowPrivate bool
	mu           sync.Mutex
	inFlight     map[int64]bool
}

// New builds the service.
func New(bio BucketIO, store Store, allowPrivate bool) *Service {
	return &Service{io: bio, store: store, allowPrivate: allowPrivate, inFlight: map[int64]bool{}}
}

// SetNotifier wires failure notifications.
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// InFlight reports how many backups or restores are running (the self-updater
// refuses to restart Krill while any are).
func (s *Service) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inFlight)
}

func (s *Service) claim(id int64) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[id] {
		return nil, false
	}
	s.inFlight[id] = true
	return func() { s.mu.Lock(); delete(s.inFlight, id); s.mu.Unlock() }, true
}

// baseDir is the directory holding every snapshot of the target's bucket.
func baseDir(prefix string, t Target) string {
	d := t.Server + "/" + t.Bucket + "/"
	if prefix = strings.Trim(prefix, "/"); prefix != "" {
		d = prefix + "/" + d
	}
	return d
}

// StartBackup runs a backup in the background on a detached context bounded
// by timeout; ErrRunning when one of this configuration is already running.
func (s *Service) StartBackup(id int64, now time.Time, timeout time.Duration) error {
	release, ok := s.claim(id)
	if !ok {
		return ErrRunning
	}
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := s.runBackup(ctx, id, now); err != nil {
			slog.Error("bucket backup failed", "err", err, "bucket_backup_id", id)
		}
	}()
	return nil
}

// RunBackup is the scheduler's synchronous form of StartBackup.
func (s *Service) RunBackup(ctx context.Context, id int64, now time.Time) error {
	release, ok := s.claim(id)
	if !ok {
		return ErrRunning
	}
	defer release()
	return s.runBackup(ctx, id, now)
}

func (s *Service) runBackup(ctx context.Context, id int64, now time.Time) error {
	row, t, dst, err := s.resolve(ctx, id)
	if err != nil {
		return s.fail(ctx, id, now, err)
	}
	entries, err := s.io.List(ctx, t)
	if err != nil {
		return s.fail(ctx, id, now, fmt.Errorf("list bucket: %w", err))
	}
	dir := baseDir(row.Prefix, t) + now.UTC().Format(tsLayout) + "/"
	m := Manifest{Version: 1, Server: t.Server, Bucket: t.Bucket, Taken: now.UTC()}
	for _, e := range entries {
		if !SafeKey(e.Key) {
			return s.fail(ctx, id, now, fmt.Errorf("%w: %q", ErrUnsafeKey, e.Key))
		}
		sum, size, err := s.copyOut(ctx, t, dst, e.Key, dir+"objects/"+e.Key)
		if err != nil {
			return s.fail(ctx, id, now, fmt.Errorf("copy %q: %w", e.Key, err))
		}
		m.Objects = append(m.Objects, ManifestObject{Key: e.Key, Size: size, SHA256: sum})
	}
	body, _ := json.MarshalIndent(m, "", "  ")
	// The manifest goes last: its presence is what makes the snapshot complete.
	if err := backup.Upload(ctx, dst, dir+manifestName, bytes.NewReader(body), s.allowPrivate); err != nil {
		return s.fail(ctx, id, now, fmt.Errorf("write manifest: %w", err))
	}
	s.prune(ctx, dst, baseDir(row.Prefix, t), row.Retention)
	return s.store.SetBackupResult(ctx, id, now, "ok", "")
}

// copyOut streams one object from the bucket into backup storage, hashing it
// on the way.
func (s *Service) copyOut(ctx context.Context, t Target, dst backup.Destination, key, storeKey string) (string, int64, error) {
	pr, pw := io.Pipe()
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(pw, h)}
	go func() {
		pw.CloseWithError(s.io.Read(ctx, t, key, cw))
	}()
	if err := backup.Upload(ctx, dst, storeKey, pr, s.allowPrivate); err != nil {
		pr.CloseWithError(err)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// prune keeps the newest keep complete snapshots and removes older ones,
// along with incomplete snapshots older than the newest complete one (a run
// that failed half-way). Failures are logged, never fatal to the backup.
func (s *Service) prune(ctx context.Context, dst backup.Destination, base string, keep int) {
	snaps, objs, err := s.scan(ctx, dst, base)
	if err != nil {
		slog.Warn("bucket backup retention: list failed", "err", err)
		return
	}
	if keep < 1 {
		keep = 1
	}
	var complete []string
	for _, sn := range snaps {
		if sn.Complete {
			complete = append(complete, sn.Timestamp)
		}
	}
	if len(complete) == 0 {
		return
	}
	newest := complete[0]
	drop := map[string]bool{}
	for i, ts := range complete {
		if i >= keep {
			drop[ts] = true
		}
	}
	for _, sn := range snaps {
		if !sn.Complete && sn.Timestamp < newest {
			drop[sn.Timestamp] = true
		}
	}
	for _, o := range objs {
		if ts := snapshotOf(base, o.Key); drop[ts] {
			if err := backup.Delete(ctx, dst, o.Key, s.allowPrivate); err != nil {
				slog.Warn("bucket backup retention: delete failed", "key", o.Key, "err", err)
			}
		}
	}
}

// snapshotOf returns the timestamp directory a stored key belongs to.
func snapshotOf(base, key string) string {
	rest := strings.TrimPrefix(key, base)
	if i := strings.IndexByte(rest, '/'); i > 0 {
		return rest[:i]
	}
	return ""
}

// scan lists the snapshots under base, newest first.
func (s *Service) scan(ctx context.Context, dst backup.Destination, base string) ([]Snapshot, []backup.Object, error) {
	objs, err := backup.List(ctx, dst, base, s.allowPrivate)
	if err != nil {
		return nil, nil, err
	}
	by := map[string]*Snapshot{}
	for _, o := range objs {
		ts := snapshotOf(base, o.Key)
		if !tsRe.MatchString(ts) {
			continue
		}
		sn := by[ts]
		if sn == nil {
			sn = &Snapshot{Timestamp: ts}
			by[ts] = sn
		}
		rest := strings.TrimPrefix(o.Key, base+ts+"/")
		switch {
		case rest == manifestName:
			sn.Complete = true
		case strings.HasPrefix(rest, "objects/"):
			sn.Objects++
			sn.Bytes += o.Size
		}
	}
	out := make([]Snapshot, 0, len(by))
	for _, sn := range by {
		out = append(out, *sn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	return out, objs, nil
}

// Snapshots lists a configuration's snapshots, newest first.
func (s *Service) Snapshots(ctx context.Context, id int64) ([]Snapshot, error) {
	row, t, dst, err := s.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	snaps, _, err := s.scan(ctx, dst, baseDir(row.Prefix, t))
	return snaps, err
}

// StartRestore restores snapshot ts of configuration id into the empty bucket
// target, in the background. It is checked synchronously first, so a caller
// learns right away about a missing snapshot or a non-empty target.
func (s *Service) StartRestore(ctx context.Context, id int64, ts string, target Target, timeout time.Duration) error {
	if !tsRe.MatchString(ts) {
		return ErrNoSnapshot
	}
	release, ok := s.claim(id)
	if !ok {
		return ErrRunning
	}
	row, src, dst, err := s.resolve(ctx, id)
	if err == nil {
		err = s.checkRestore(ctx, row, src, dst, ts, target)
	}
	if err != nil {
		release()
		return err
	}
	go func() {
		defer release()
		rctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		now := time.Now()
		if err := s.restore(rctx, row, src, dst, ts, target); err != nil {
			slog.Error("bucket restore failed", "err", err, "bucket_backup_id", id, "snapshot", ts)
			_ = s.store.SetRestoreResult(context.WithoutCancel(rctx), id, now, "error", err.Error())
			if s.notifier != nil {
				s.notifier.BackupFailed(rctx, id, "restore: "+err.Error())
			}
			return
		}
		_ = s.store.SetRestoreResult(context.WithoutCancel(rctx), id, now, "ok", "")
	}()
	return nil
}

func (s *Service) checkRestore(ctx context.Context, row Row, src Target, dst backup.Destination, ts string, target Target) error {
	snaps, _, err := s.scan(ctx, dst, baseDir(row.Prefix, src))
	if err != nil {
		return err
	}
	found := false
	for _, sn := range snaps {
		found = found || (sn.Timestamp == ts && sn.Complete)
	}
	if !found {
		return ErrNoSnapshot
	}
	existing, err := s.io.List(ctx, target)
	if err != nil {
		return fmt.Errorf("list target bucket: %w", err)
	}
	if len(existing) > 0 {
		return ErrTargetNotEmpty
	}
	return nil
}

// restore copies every object of the snapshot into target, checking each
// against the manifest, then compares the target's listing with it.
func (s *Service) restore(ctx context.Context, row Row, src Target, dst backup.Destination, ts string, target Target) error {
	dir := baseDir(row.Prefix, src) + ts + "/"
	rc, err := backup.Download(ctx, dst, dir+manifestName, s.allowPrivate)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	derr := json.NewDecoder(rc).Decode(&m)
	rc.Close()
	if derr != nil {
		return fmt.Errorf("parse manifest: %w", derr)
	}
	for _, o := range m.Objects {
		if !SafeKey(o.Key) {
			return fmt.Errorf("%w: %q", ErrUnsafeKey, o.Key)
		}
	}
	for _, o := range m.Objects {
		body, err := backup.Download(ctx, dst, dir+"objects/"+o.Key, s.allowPrivate)
		if err != nil {
			return fmt.Errorf("read %q: %w", o.Key, err)
		}
		h := sha256.New()
		cr := &countReader{r: io.TeeReader(body, h)}
		werr := s.io.Write(ctx, target, o.Key, cr)
		body.Close()
		if werr != nil {
			return fmt.Errorf("write %q: %w", o.Key, werr)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != o.SHA256 || cr.n != o.Size {
			return fmt.Errorf("object %q does not match the manifest (sha256 %s, %d bytes; want %s, %d bytes)", o.Key, got, cr.n, o.SHA256, o.Size)
		}
	}
	restored, err := s.io.List(ctx, target)
	if err != nil {
		return fmt.Errorf("list restored bucket: %w", err)
	}
	want := map[string]int64{}
	for _, o := range m.Objects {
		want[o.Key] = o.Size
	}
	if len(restored) != len(want) {
		return fmt.Errorf("restored bucket holds %d objects, the manifest %d", len(restored), len(want))
	}
	for _, e := range restored {
		if size, ok := want[e.Key]; !ok || size != e.Size {
			return fmt.Errorf("restored object %q does not match the manifest", e.Key)
		}
	}
	return nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Service) resolve(ctx context.Context, id int64) (Row, Target, backup.Destination, error) {
	row, err := s.store.GetBackup(ctx, id)
	if err != nil {
		return Row{}, Target{}, backup.Destination{}, err
	}
	t, err := s.store.GetTarget(ctx, row.BucketID)
	if err != nil {
		return Row{}, Target{}, backup.Destination{}, err
	}
	dst, err := s.store.GetDestination(ctx, row.DestinationID)
	if err != nil {
		return Row{}, Target{}, backup.Destination{}, err
	}
	return row, t, dst, nil
}

func (s *Service) fail(ctx context.Context, id int64, now time.Time, err error) error {
	_ = s.store.SetBackupResult(context.WithoutCancel(ctx), id, now, "error", err.Error())
	if s.notifier != nil {
		s.notifier.BackupFailed(ctx, id, err.Error())
	}
	return err
}
