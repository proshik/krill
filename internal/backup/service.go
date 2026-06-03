package backup

import (
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"time"
)

// Execer runs a command inside a managed DB's container (docker.Engine satisfies it).
type Execer interface {
	Exec(ctx context.Context, serviceName string, cmd []string, env []string, stdin io.Reader, stdout io.Writer) error
}

type Service struct {
	eng   Execer
	store Store
}

func New(eng Execer, store Store) *Service { return &Service{eng: eng, store: store} }

func objectsToDelete(objs []Object, keep int) []Object {
	if keep < 1 {
		keep = 1
	}
	if len(objs) <= keep {
		return nil
	}
	return objs[keep:]
}

func prefixDir(prefix, appName string) string {
	if prefix != "" {
		return prefix + "/" + appName + "/"
	}
	return appName + "/"
}

func keyFor(prefix, appName string, now time.Time) string {
	return prefixDir(prefix, appName) + now.UTC().Format("2006-01-02T15-04-05Z") + ".sql.gz"
}

// RunBackup dumps the DB, uploads it, then enforces count-based retention.
func (s *Service) RunBackup(ctx context.Context, backupID int64, now time.Time) error {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return err
	}
	pg, err := s.store.GetPGTarget(ctx, b.PostgresDbID)
	if err != nil {
		return s.fail(ctx, backupID, now, err)
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return s.fail(ctx, backupID, now, err)
	}
	key := keyFor(b.Prefix, pg.AppName, now)

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
	if uerr := Upload(ctx, dst, key, pr); uerr != nil {
		return s.fail(ctx, backupID, now, uerr)
	}

	if objs, lerr := List(ctx, dst, prefixDir(b.Prefix, pg.AppName)); lerr == nil {
		for _, o := range objectsToDelete(objs, b.Retention) {
			if derr := Delete(ctx, dst, o.Key); derr != nil {
				slog.Warn("backup retention delete failed", "key", o.Key, "err", derr)
			}
		}
	} else {
		slog.Warn("backup retention list failed", "err", lerr)
	}
	return s.store.SetBackupResult(ctx, backupID, now, "ok", "")
}

func (s *Service) fail(ctx context.Context, id int64, now time.Time, err error) error {
	_ = s.store.SetBackupResult(ctx, id, now, "error", err.Error())
	return err
}

// ListObjects lists the stored backups for a backup config (its destination + prefix dir).
func (s *Service) ListObjects(ctx context.Context, backupID int64) ([]Object, error) {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return nil, err
	}
	pg, err := s.store.GetPGTarget(ctx, b.PostgresDbID)
	if err != nil {
		return nil, err
	}
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return nil, err
	}
	return List(ctx, dst, prefixDir(b.Prefix, pg.AppName))
}

// RestoreByID restores object `key` of a backup config into its DB.
func (s *Service) RestoreByID(ctx context.Context, backupID int64, key string) error {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return err
	}
	pg, err := s.store.GetPGTarget(ctx, b.PostgresDbID)
	if err != nil {
		return err
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
	dst, err := s.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return nil, err
	}
	return Download(ctx, dst, key)
}

// Restore streams a stored backup from S3 through gunzip into psql.
func (s *Service) Restore(ctx context.Context, dst Destination, pg PGTarget, key string) error {
	rc, err := Download(ctx, dst, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	defer gz.Close()
	return s.eng.Exec(ctx, pg.AppName,
		[]string{"psql", "-v", "ON_ERROR_STOP=1", "-U", pg.DatabaseUser, "-d", pg.DatabaseName},
		[]string{"PGPASSWORD=" + pg.DatabasePassword}, gz, io.Discard)
}
