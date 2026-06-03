package backup

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/proshik/krill/internal/database/gen"
)

// PGTarget is the connection info for dumping/restoring a managed Postgres DB.
type PGTarget struct {
	AppName          string
	DatabaseName     string
	DatabaseUser     string
	DatabasePassword string
}

// BackupRow is the minimal backup config the service needs.
type BackupRow struct {
	ID            int64
	PostgresDbID  int64
	DestinationID int64
	Prefix        string
	Retention     int
}

// Store is what the backup Service needs from persistence.
type Store interface {
	GetBackup(ctx context.Context, id int64) (BackupRow, error)
	GetPGTarget(ctx context.Context, pgID int64) (PGTarget, error)
	GetDestination(ctx context.Context, id int64) (Destination, error)
	SetBackupResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error
}

// DBStore implements Store over sqlc.
type DBStore struct{ q *db.Queries }

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) GetBackup(ctx context.Context, id int64) (BackupRow, error) {
	b, err := s.q.GetBackup(ctx, id)
	if err != nil {
		return BackupRow{}, err
	}
	return BackupRow{ID: b.ID, PostgresDbID: b.PostgresDbID, DestinationID: b.DestinationID, Prefix: b.Prefix, Retention: int(b.Retention)}, nil
}

func (s *DBStore) GetPGTarget(ctx context.Context, pgID int64) (PGTarget, error) {
	pg, err := s.q.GetPostgres(ctx, pgID)
	if err != nil {
		return PGTarget{}, err
	}
	return PGTarget{AppName: pg.AppName, DatabaseName: pg.DatabaseName, DatabaseUser: pg.DatabaseUser, DatabasePassword: pg.DatabasePassword}, nil
}

func (s *DBStore) GetDestination(ctx context.Context, id int64) (Destination, error) {
	d, err := s.q.GetDestination(ctx, id)
	if err != nil {
		return Destination{}, err
	}
	return Destination{Endpoint: d.Endpoint, Bucket: d.Bucket, Region: d.Region, AccessKey: d.AccessKey, SecretKey: d.SecretKey}, nil
}

func (s *DBStore) SetBackupResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error {
	return s.q.SetBackupResult(ctx, db.SetBackupResultParams{
		ID: id, LastRunAt: pgtype.Timestamptz{Time: at, Valid: true}, LastStatus: status, LastError: errMsg,
	})
}
