package bucketbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/testutil"
)

// memIO is BucketIO over in-memory buckets keyed by name.
type memIO struct {
	mu      sync.Mutex
	buckets map[string]map[string][]byte
}

func (m *memIO) List(_ context.Context, t Target) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for k, v := range m.buckets[t.Bucket] {
		out = append(out, Entry{Key: k, Size: int64(len(v))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memIO) Read(_ context.Context, t Target, key string, w io.Writer) error {
	m.mu.Lock()
	b, ok := m.buckets[t.Bucket][key]
	m.mu.Unlock()
	if !ok {
		return errors.New("no such key")
	}
	_, err := w.Write(b)
	return err
}

func (m *memIO) Write(_ context.Context, t Target, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buckets[t.Bucket] == nil {
		m.buckets[t.Bucket] = map[string][]byte{}
	}
	m.buckets[t.Bucket][key] = b
	return nil
}

type memStore struct {
	mu      sync.Mutex
	row     Row
	targets map[int64]Target
	dst     backup.Destination
	status  string
	restore string
	errMsg  string
}

func (s *memStore) GetBackup(context.Context, int64) (Row, error) { return s.row, nil }
func (s *memStore) GetTarget(_ context.Context, id int64) (Target, error) {
	return s.targets[id], nil
}
func (s *memStore) GetDestination(context.Context, int64) (backup.Destination, error) {
	return s.dst, nil
}
func (s *memStore) SetBackupResult(_ context.Context, _ int64, _ time.Time, status, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.errMsg = status, msg
	return nil
}
func (s *memStore) SetRestoreResult(_ context.Context, _ int64, _ time.Time, status, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restore, s.errMsg = status, msg
	return nil
}
func (s *memStore) restoreStatus() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restore, s.errMsg
}

func fixture(t *testing.T) (*Service, *memIO, *memStore) {
	t.Helper()
	m := testutil.NewMinio(t)
	dst := backup.Destination{Endpoint: m.Endpoint, Bucket: "backups", Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	if err := backup.CreateBucket(context.Background(), dst, true); err != nil {
		t.Fatal(err)
	}
	bio := &memIO{buckets: map[string]map[string][]byte{"penpot": {
		"objects/a.png":        bytes.Repeat([]byte("a"), 3000),
		"dir with space/b.bin": []byte("hello"),
		"empty":                {},
	}}}
	st := &memStore{
		row:     Row{ID: 1, BucketID: 7, DestinationID: 3, Prefix: "krill", Retention: 2},
		targets: map[int64]Target{7: {Server: "krill-minio-s3", Bucket: "penpot"}},
		dst:     dst,
	}
	return New(bio, st, true), bio, st
}

func TestBackupWritesObjectsAndManifestThenRestoresIntoAnEmptyBucket(t *testing.T) {
	svc, bio, st := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	if err := svc.RunBackup(ctx, 1, now); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if st.status != "ok" {
		t.Fatalf("status %q %q", st.status, st.errMsg)
	}
	snaps, err := svc.Snapshots(ctx, 1)
	if err != nil || len(snaps) != 1 || !snaps[0].Complete || snaps[0].Objects != 3 || snaps[0].Bytes != 3005 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	rc, err := backup.Download(ctx, st.dst, "krill/krill-minio-s3/penpot/2026-11-07T10-00-00Z/manifest.json", true)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	json.NewDecoder(rc).Decode(&m)
	rc.Close()
	if len(m.Objects) != 3 || m.Objects[0].SHA256 == "" {
		t.Fatalf("manifest: %+v", m)
	}

	// A restore into a bucket that already holds data is refused up front.
	target := Target{Server: "krill-minio-s3", Bucket: "penpot-restored"}
	bio.buckets["penpot-restored"] = map[string][]byte{"keep-me": []byte("x")}
	if err := svc.StartRestore(ctx, 1, snaps[0].Timestamp, target, time.Minute); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("non-empty target: %v", err)
	}
	delete(bio.buckets, "penpot-restored")
	if err := svc.StartRestore(ctx, 1, "2026-01-01T00-00-00Z", target, time.Minute); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("missing snapshot: %v", err)
	}
	if err := svc.StartRestore(ctx, 1, snaps[0].Timestamp, target, time.Minute); err != nil {
		t.Fatalf("restore: %v", err)
	}
	waitRestore(t, st)
	if status, msg := st.restoreStatus(); status != "ok" {
		t.Fatalf("restore status %q: %s", status, msg)
	}
	for k, v := range bio.buckets["penpot"] {
		if !bytes.Equal(bio.buckets["penpot-restored"][k], v) {
			t.Fatalf("object %q differs after restore", k)
		}
	}
	if len(bio.buckets["penpot-restored"]) != 3 {
		t.Fatalf("restored %d objects", len(bio.buckets["penpot-restored"]))
	}
}

// Retention keeps the newest N complete snapshots and drops older ones, and a
// half-written snapshot older than the newest complete one.
func TestRetentionKeepsNewestCompleteSnapshots(t *testing.T) {
	svc, _, st := fixture(t)
	ctx := context.Background()
	// A run that died before its manifest.
	if err := backup.Upload(ctx, st.dst, "krill/krill-minio-s3/penpot/2026-11-07T09-00-00Z/objects/x", strings.NewReader("x"), true); err != nil {
		t.Fatal(err)
	}
	for i, h := range []int{10, 11, 12} {
		if err := svc.RunBackup(ctx, 1, time.Date(2026, 11, 7, h, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	snaps, _ := svc.Snapshots(ctx, 1)
	var got []string
	for _, s := range snaps {
		got = append(got, s.Timestamp)
	}
	if strings.Join(got, ",") != "2026-11-07T12-00-00Z,2026-11-07T11-00-00Z" {
		t.Fatalf("snapshots after retention 2: %v", got)
	}
}

// An object changed in backup storage is caught by its sha256, and a manifest
// key that would leave the target bucket is refused before anything is written.
func TestRestoreRejectsTamperedSnapshots(t *testing.T) {
	svc, bio, st := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	if err := svc.RunBackup(ctx, 1, now); err != nil {
		t.Fatal(err)
	}
	dir := "krill/krill-minio-s3/penpot/2026-11-07T10-00-00Z/"
	if err := backup.Upload(ctx, st.dst, dir+"objects/dir with space/b.bin", strings.NewReader("HELLO"), true); err != nil {
		t.Fatal(err)
	}
	target := Target{Server: "krill-minio-s3", Bucket: "restored-1"}
	if err := svc.StartRestore(ctx, 1, "2026-11-07T10-00-00Z", target, time.Minute); err != nil {
		t.Fatal(err)
	}
	waitRestore(t, st)
	if status, msg := st.restoreStatus(); status != "error" || !strings.Contains(msg, "does not match the manifest") {
		t.Fatalf("tampered object: %q %q", status, msg)
	}

	evil, _ := json.Marshal(Manifest{Version: 1, Objects: []ManifestObject{{Key: "../other-bucket/pwned", Size: 1, SHA256: "x"}}})
	if err := backup.Upload(ctx, st.dst, dir+manifestName, bytes.NewReader(evil), true); err != nil {
		t.Fatal(err)
	}
	st.restore = ""
	target2 := Target{Server: "krill-minio-s3", Bucket: "restored-2"}
	if err := svc.StartRestore(ctx, 1, "2026-11-07T10-00-00Z", target2, time.Minute); err != nil {
		t.Fatal(err)
	}
	waitRestore(t, st)
	if status, msg := st.restoreStatus(); status != "error" || !strings.Contains(msg, "not a plain path") {
		t.Fatalf("unsafe key: %q %q", status, msg)
	}
	if len(bio.buckets["restored-2"]) != 0 || len(bio.buckets["other-bucket"]) != 0 {
		t.Fatal("an unsafe manifest must write nothing")
	}
}

func TestSafeKey(t *testing.T) {
	for k, ok := range map[string]bool{
		"a.png": true, "dir/sub/file 1.txt": true, "..a/b": true,
		"": false, "/abs": false, "a/../b": false, "../x": false, "a//b": false, "./a": false, "a\\b": false, "a/": false,
	} {
		if SafeKey(k) != ok {
			t.Errorf("SafeKey(%q) != %v", k, ok)
		}
	}
}

func waitRestore(t *testing.T, st *memStore) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if s, _ := st.restoreStatus(); s != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("restore did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
