package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/oplock"
)

// ErrKeyOutsideBackup is returned when a requested object key is not within the
// backup's own prefix — prevents downloading/restoring arbitrary bucket objects.
var ErrKeyOutsideBackup = errors.New("object key is outside this backup's prefix")

// ErrBackupRunning is returned when a backup is triggered while a previous run
// of the SAME backup is still in flight. Cron-level SkipIfStillRunning cannot
// cover this: Scheduler.Reload swaps in a fresh cron instance (losing the
// wrapper's state), and the manual "Backup now" path bypasses cron entirely.
var ErrBackupRunning = errors.New("backup already running")

// Execer runs a command inside a managed DB's container (docker.Engine satisfies it).
type Execer interface {
	Exec(ctx context.Context, serviceName string, cmd []string, env []string, stdin io.Reader, stdout io.Writer) error
}

// Notifier is the optional sink for backup-failure alerts (implemented by
// *notify.Service). Defined here to avoid importing the notify package.
type Notifier interface {
	BackupFailed(ctx context.Context, backupID int64, reason string)
}

type Service struct {
	eng          Execer
	store        Store
	notifier     Notifier
	allowPrivate bool // KRILL_ALLOW_PRIVATE_EGRESS: disables the SSRF egress guard on S3 traffic

	mu       sync.Mutex
	inFlight map[int64]bool // backup IDs with a run in progress
}

func New(eng Execer, store Store, allowPrivate bool) *Service {
	return &Service{eng: eng, store: store, allowPrivate: allowPrivate, inFlight: map[int64]bool{}}
}

// SetNotifier wires backup-failure notifications (no-op if never set).
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// InFlight reports how many backups or restores are running right now. The
// self-updater refuses to restart Krill while any are.
func (s *Service) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inFlight)
}

func objectsToDelete(objs []Object, keep int) []Object {
	if keep < 1 {
		keep = 1
	}
	if len(objs) <= keep {
		return nil
	}
	return objs[keep:]
}

// prefixDir: one directory per logical database — retention must not mix dumps
// of different databases sharing an instance. Legacy dumps stay under the old
// <prefix>/<appName>/ path: intact in the bucket, but no longer listed by the UI
// nor counted by retention (documented one-time conversion cost).
func prefixDir(prefix, appName, dbName string) string {
	if prefix != "" {
		return prefix + "/" + appName + "/" + dbName + "/"
	}
	return appName + "/" + dbName + "/"
}

func keyFor(prefix, appName, dbName string, now time.Time) string {
	return prefixDir(prefix, appName, dbName) + now.UTC().Format("2006-01-02T15-04-05Z") + ".sql.gz"
}

// claim reserves backupID for one backup or restore. It returns false when
// one is already running; otherwise release must be called when it ends.
func (s *Service) claim(backupID int64) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[backupID] {
		return nil, false
	}
	s.inFlight[backupID] = true
	return func() {
		s.mu.Lock()
		delete(s.inFlight, backupID)
		s.mu.Unlock()
	}, true
}

// start claims backupID and runs op in the background on a detached context
// bounded by timeout, so the caller learns right away whether it started —
// a request that reported "started" and then lost the claim inside the
// goroutine would leave the operator believing a second run was under way.
func (s *Service) start(backupID int64, timeout time.Duration, what string, op func(context.Context) error) error {
	release, ok := s.claim(backupID)
	if !ok {
		return ErrBackupRunning
	}
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := op(ctx); err != nil {
			slog.Error(what+" failed", "err", err, "backup_id", backupID)
		}
	}()
	return nil
}

// StartBackup is RunBackup in the background: ErrBackupRunning when a backup
// or restore of backupID is already running, nil once this one has started.
func (s *Service) StartBackup(backupID int64, now time.Time, timeout time.Duration) error {
	return s.start(backupID, timeout, "backup", func(ctx context.Context) error {
		return s.runBackup(ctx, backupID, now)
	})
}

// StartRestore is RestoreByID in the background, with StartBackup's contract.
func (s *Service) StartRestore(backupID int64, key string, timeout time.Duration) error {
	return s.start(backupID, timeout, "restore", func(ctx context.Context) error {
		return s.restoreByID(ctx, backupID, key)
	})
}

// RunBackup dumps the DB, uploads it, then enforces count-based retention.
// Overlapping runs of the same backup are rejected with ErrBackupRunning (two
// concurrent pg_dumps + uploads of one DB racing retention deletes).
func (s *Service) RunBackup(ctx context.Context, backupID int64, now time.Time) error {
	release, ok := s.claim(backupID)
	if !ok {
		return ErrBackupRunning
	}
	defer release()
	return s.runBackup(ctx, backupID, now)
}

func (s *Service) runBackup(ctx context.Context, backupID int64, now time.Time) error {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return err
	}
	pg, err := s.store.GetPGTarget(ctx, b.LogicalDatabaseID)
	if err != nil {
		return s.fail(ctx, backupID, now, err)
	}
	if oplock.Held(oplock.DBInstance(pg.AppName)) {
		return s.fail(ctx, backupID, now, fmt.Errorf("db instance %s is migrating — retry after the migration finishes", pg.AppName))
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return s.fail(ctx, backupID, now, err)
	}
	key := keyFor(b.Prefix, pg.AppName, pg.DatabaseName, now)

	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		if eerr := s.eng.Exec(ctx, pg.AppName,
			[]string{"pg_dump", "--clean", "--if-exists", "-U", pg.DatabaseUser, "-d", pg.DatabaseName},
			[]string{"PGPASSWORD=" + pg.DatabasePassword}, nil, gz); eerr != nil {
			_ = gz.Close()
			pw.CloseWithError(eerr)
			return
		}
		if cerr := gz.Close(); cerr != nil {
			pw.CloseWithError(cerr)
			return
		}
		pw.Close()
	}()
	if uerr := Upload(ctx, dst, key, pr, s.allowPrivate); uerr != nil {
		// Close the read side so the dump goroutine's blocked pw.Write fails and
		// the goroutine (and its docker exec + in-container pg_dump) terminates —
		// otherwise each failed upload leaks them all until process exit.
		pr.CloseWithError(uerr)
		return s.fail(ctx, backupID, now, uerr)
	}

	// Retention runs after a successful upload, so a failure here does not make
	// the run a failure — the dump is stored. It does mean the bucket keeps
	// growing, which must not be invisible: record it alongside the "ok".
	objs, lerr := List(ctx, dst, prefixDir(b.Prefix, pg.AppName, pg.DatabaseName), s.allowPrivate)
	deleteFailures := 0
	if lerr != nil {
		slog.Error("backup retention list failed — old backups not pruned", "backup", backupID, "err", lerr)
	} else {
		for _, o := range objectsToDelete(objs, b.Retention) {
			if derr := Delete(ctx, dst, o.Key, s.allowPrivate); derr != nil {
				deleteFailures++
				slog.Error("backup retention delete failed", "backup", backupID, "key", o.Key, "err", derr)
			}
		}
	}
	return s.store.SetBackupResult(ctx, backupID, now, "ok", retentionNote(lerr, deleteFailures))
}

// retentionNote describes a retention pass that ran after a successful upload.
// Empty when the prune was clean; otherwise a message for the backup's
// last_error, which the UI renders under the run status.
func retentionNote(listErr error, deleteFailures int) string {
	if listErr != nil {
		return fmt.Sprintf("backup stored, but old backups could not be pruned (listing failed: %v) — the bucket will keep growing", listErr)
	}
	if deleteFailures > 0 {
		return fmt.Sprintf("backup stored, but %d old backup(s) could not be deleted — the bucket will keep growing", deleteFailures)
	}
	return ""
}

func (s *Service) fail(ctx context.Context, id int64, now time.Time, err error) error {
	_ = s.store.SetBackupResult(ctx, id, now, "error", err.Error())
	if s.notifier != nil {
		s.notifier.BackupFailed(ctx, id, err.Error())
	}
	return err
}

// ListObjects lists the stored backups for a backup config (its destination + prefix dir).
func (s *Service) ListObjects(ctx context.Context, backupID int64) ([]Object, error) {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return nil, err
	}
	pg, err := s.store.GetPGTarget(ctx, b.LogicalDatabaseID)
	if err != nil {
		return nil, err
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return nil, err
	}
	return List(ctx, dst, prefixDir(b.Prefix, pg.AppName, pg.DatabaseName), s.allowPrivate)
}

// RestoreByID restores object `key` of a backup config into its DB.
func (s *Service) RestoreByID(ctx context.Context, backupID int64, key string) error {
	// Share the backup's in-flight guard: restoring into a database while a
	// pg_dump of it is streaming produces a dump of a half-restored database.
	release, ok := s.claim(backupID)
	if !ok {
		return ErrBackupRunning
	}
	defer release()
	return s.restoreByID(ctx, backupID, key)
}

func (s *Service) restoreByID(ctx context.Context, backupID int64, key string) error {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return err
	}
	pg, err := s.store.GetPGTarget(ctx, b.LogicalDatabaseID)
	if err != nil {
		return err
	}
	if oplock.Held(oplock.DBInstance(pg.AppName)) {
		return fmt.Errorf("db instance %s is migrating — retry after the migration finishes", pg.AppName)
	}
	if !strings.HasPrefix(key, prefixDir(b.Prefix, pg.AppName, pg.DatabaseName)) {
		return ErrKeyOutsideBackup
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return err
	}
	return s.Restore(ctx, dst, pg, key)
}

// OpenObject opens a stored backup object for streaming download.
func (s *Service) OpenObject(ctx context.Context, backupID int64, key string) (io.ReadCloser, error) {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return nil, err
	}
	pg, err := s.store.GetPGTarget(ctx, b.LogicalDatabaseID)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(key, prefixDir(b.Prefix, pg.AppName, pg.DatabaseName)) {
		return nil, ErrKeyOutsideBackup
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return nil, err
	}
	return Download(ctx, dst, key, s.allowPrivate)
}

// Restore streams a stored backup from S3 through gunzip into psql.
func (s *Service) Restore(ctx context.Context, dst Destination, pg PGTarget, key string) error {
	rc, err := Download(ctx, dst, key, s.allowPrivate)
	if err != nil {
		return err
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	defer gz.Close()
	// --single-transaction makes the restore atomic: the dump starts with
	// `pg_dump --clean` DROP statements, so without it a mid-stream failure
	// (broken S3 download, corrupt gzip, bad statement) would commit the drops
	// and leave the database half-restored. With it, any failure rolls back to
	// the pre-restore state (plain pg_dump output is fully transactional).
	return s.eng.Exec(ctx, pg.AppName,
		[]string{"psql", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-U", pg.DatabaseUser, "-d", pg.DatabaseName},
		[]string{"PGPASSWORD=" + pg.DatabasePassword}, gz, io.Discard)
}
