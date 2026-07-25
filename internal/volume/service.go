package volume

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/docker"
)

var (
	ErrVolumeBackupRunning    = errors.New("volume backup already running")
	ErrKeyOutsideVolumeBackup = errors.New("object key is outside this volume backup's prefix")
)

// quiesceTimeout bounds how long restore waits for the app to scale to 0.
const quiesceTimeout = 60 * time.Second

// VolEngine is the slice of docker.Engine the volume backup service needs.
type VolEngine interface {
	VolumeArchive(ctx context.Context, volumeName string, out io.Writer, swarmNodeID string) error
	VolumeRestore(ctx context.Context, volumeName string, in io.Reader, swarmNodeID string) error
	ServiceScale(ctx context.Context, name string, replicas uint64) error
	ServiceState(ctx context.Context, name string) (docker.ServiceState, error)
}

// Notifier is the optional backup-failure sink (implemented by *notify.Service).
type Notifier interface {
	BackupFailed(ctx context.Context, backupID int64, reason string)
}

type VolumeService struct {
	eng          VolEngine
	store        VolumeStore
	notifier     Notifier
	allowPrivate bool // KRILL_ALLOW_PRIVATE_EGRESS: disables the SSRF egress guard on S3 traffic
	mu           sync.Mutex
	inFlight     map[int64]bool
}

func New(eng VolEngine, store VolumeStore, allowPrivate bool) *VolumeService {
	return &VolumeService{eng: eng, store: store, allowPrivate: allowPrivate, inFlight: map[int64]bool{}}
}

func (s *VolumeService) SetNotifier(n Notifier) { s.notifier = n }

func volKey(appID int64, volumeName string) string {
	return strconv.FormatInt(appID, 10) + "-" + volumeName
}

func prefixDir(prefix, key string) string {
	if prefix != "" {
		return prefix + "/" + key + "/"
	}
	return key + "/"
}

func keyFor(prefix, key string, now time.Time) string {
	return prefixDir(prefix, key) + now.UTC().Format("2006-01-02T15-04-05Z") + ".tar.gz"
}

func objectsToDelete(objs []backup.Object, keep int) []backup.Object {
	if keep < 1 {
		keep = 1
	}
	if len(objs) <= keep {
		return nil
	}
	return objs[keep:]
}

// RunVolumeBackup archives the volume (hot, read-only mount), uploads it, then
// enforces count-based retention. Overlapping runs of the same backup are rejected.
func (s *VolumeService) RunVolumeBackup(ctx context.Context, volBackupID int64, now time.Time) error {
	s.mu.Lock()
	if s.inFlight[volBackupID] {
		s.mu.Unlock()
		return ErrVolumeBackupRunning
	}
	s.inFlight[volBackupID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.inFlight, volBackupID); s.mu.Unlock() }()

	b, err := s.store.GetVolumeBackup(ctx, volBackupID)
	if err != nil {
		return err
	}
	t, err := s.store.GetVolTarget(ctx, b.AppVolumeID)
	if err != nil {
		return s.fail(ctx, volBackupID, now, err)
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return s.fail(ctx, volBackupID, now, err)
	}
	key := keyFor(b.Prefix, volKey(t.AppID, t.VolumeName), now)

	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		if aerr := s.eng.VolumeArchive(ctx, docker.VolumeName(t.AppID, t.VolumeName), gz, ""); aerr != nil {
			_ = gz.Close()
			pw.CloseWithError(aerr)
			return
		}
		if cerr := gz.Close(); cerr != nil {
			pw.CloseWithError(cerr)
			return
		}
		pw.Close()
	}()
	if uerr := backup.Upload(ctx, dst, key, pr, s.allowPrivate); uerr != nil {
		pr.CloseWithError(uerr)
		return s.fail(ctx, volBackupID, now, uerr)
	}
	if objs, lerr := backup.List(ctx, dst, prefixDir(b.Prefix, volKey(t.AppID, t.VolumeName)), s.allowPrivate); lerr == nil {
		for _, o := range objectsToDelete(objs, b.Retention) {
			if derr := backup.Delete(ctx, dst, o.Key, s.allowPrivate); derr != nil {
				slog.Warn("volume backup retention delete failed", "key", o.Key, "err", derr)
			}
		}
	} else {
		slog.Warn("volume backup retention list failed", "err", lerr)
	}
	return s.store.SetVolumeBackupResult(ctx, volBackupID, now, "ok", "")
}

func (s *VolumeService) fail(ctx context.Context, id int64, now time.Time, err error) error {
	_ = s.store.SetVolumeBackupResult(ctx, id, now, "error", err.Error())
	if s.notifier != nil {
		s.notifier.BackupFailed(ctx, id, err.Error())
	}
	return err
}

func (s *VolumeService) ListObjects(ctx context.Context, volBackupID int64) ([]backup.Object, error) {
	b, t, dst, err := s.resolve(ctx, volBackupID)
	if err != nil {
		return nil, err
	}
	return backup.List(ctx, dst, prefixDir(b.Prefix, volKey(t.AppID, t.VolumeName)), s.allowPrivate)
}

func (s *VolumeService) OpenObject(ctx context.Context, volBackupID int64, key string) (io.ReadCloser, error) {
	b, t, dst, err := s.resolve(ctx, volBackupID)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(key, prefixDir(b.Prefix, volKey(t.AppID, t.VolumeName))) {
		return nil, ErrKeyOutsideVolumeBackup
	}
	return backup.Download(ctx, dst, key, s.allowPrivate)
}

func (s *VolumeService) RestoreByID(ctx context.Context, volBackupID int64, key string) error {
	b, t, dst, err := s.resolve(ctx, volBackupID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(key, prefixDir(b.Prefix, volKey(t.AppID, t.VolumeName))) {
		return ErrKeyOutsideVolumeBackup
	}
	return s.Restore(ctx, dst, t, key)
}

func (s *VolumeService) resolve(ctx context.Context, volBackupID int64) (VolBackupRow, VolTarget, backup.Destination, error) {
	b, err := s.store.GetVolumeBackup(ctx, volBackupID)
	if err != nil {
		return VolBackupRow{}, VolTarget{}, backup.Destination{}, err
	}
	t, err := s.store.GetVolTarget(ctx, b.AppVolumeID)
	if err != nil {
		return VolBackupRow{}, VolTarget{}, backup.Destination{}, err
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return VolBackupRow{}, VolTarget{}, backup.Destination{}, err
	}
	return b, t, dst, nil
}

// Restore quiesces the app (scale 0 → wait Running==0), extracts the archive
// into the volume, then scales back. The scale-back runs even on failure, with
// retries + an alert, so the app is never left down.
func (s *VolumeService) Restore(ctx context.Context, dst backup.Destination, t VolTarget, key string) error {
	if err := s.eng.ServiceScale(ctx, t.ServiceName, 0); err != nil {
		return err
	}
	defer func() {
		for i := 0; i < 3; i++ {
			if err := s.eng.ServiceScale(context.WithoutCancel(ctx), t.ServiceName, t.Replicas); err == nil {
				return
			}
			time.Sleep(2 * time.Second)
		}
		slog.Error("volume restore: app left scaled to 0 (scale-up failed)", "service", t.ServiceName, "replicas", t.Replicas)
		if s.notifier != nil {
			// WithoutCancel: this alert matters most exactly when the parent ctx
			// is already dead (the restore timed out / client disconnected).
			s.notifier.BackupFailed(context.WithoutCancel(ctx), t.AppVolumeID, "volume restore: scale-up failed, app is down")
		}
	}()

	qctx, cancel := context.WithTimeout(ctx, quiesceTimeout)
	defer cancel()
	for {
		st, err := s.eng.ServiceState(qctx, t.ServiceName)
		if err != nil {
			return err
		}
		if st.Running == 0 {
			break
		}
		select {
		case <-qctx.Done():
			return errors.New("app " + t.ServiceName + " did not quiesce for restore")
		case <-time.After(500 * time.Millisecond):
		}
	}

	rc, err := backup.Download(ctx, dst, key, s.allowPrivate)
	if err != nil {
		return err
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	defer gz.Close()
	return s.eng.VolumeRestore(ctx, docker.VolumeName(t.AppID, t.VolumeName), gz, "")
}
