package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
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
	ID                int64
	LogicalDatabaseID int64
	DestinationID     int64
	Prefix            string
	Retention         int
}

// Store is what the backup Service needs from persistence.
type Store interface {
	GetBackup(ctx context.Context, id int64) (BackupRow, error)
	GetPGTarget(ctx context.Context, ldbID int64) (PGTarget, error)
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
	return BackupRow{ID: b.ID, LogicalDatabaseID: b.LogicalDatabaseID, DestinationID: b.DestinationID, Prefix: b.Prefix, Retention: int(b.Retention)}, nil
}

// GetPGTarget resolves a logical database + its instance into dump/restore
// connection info. The password is decrypted here — pg_dump/psql get the real
// value in PGPASSWORD (previously the ciphertext leaked through and only worked
// thanks to the image's trust auth for local connections).
func (s *DBStore) GetPGTarget(ctx context.Context, ldbID int64) (PGTarget, error) {
	ld, err := s.q.GetLogicalDatabase(ctx, ldbID)
	if err != nil {
		return PGTarget{}, err
	}
	inst, err := s.q.GetDBInstance(ctx, ld.InstanceID)
	if err != nil {
		return PGTarget{}, err
	}
	pw, err := secret.Dec(ld.Password)
	if err != nil {
		return PGTarget{}, fmt.Errorf("logical database %d password: %w", ld.ID, err)
	}
	return PGTarget{
		AppName:          inst.AppName,
		DatabaseName:     ld.DbName,
		DatabaseUser:     ld.Username,
		DatabasePassword: pw,
	}, nil
}

func (s *DBStore) GetDestination(ctx context.Context, id int64) (Destination, error) {
	d, err := s.q.GetDestination(ctx, id)
	if err != nil {
		return Destination{}, err
	}
	ak, err := secret.Dec(d.AccessKey)
	if err != nil {
		return Destination{}, fmt.Errorf("destination %d access key: %w", d.ID, err)
	}
	sk, err := secret.Dec(d.SecretKey)
	if err != nil {
		return Destination{}, fmt.Errorf("destination %d secret key: %w", d.ID, err)
	}
	return Destination{Endpoint: d.Endpoint, Bucket: d.Bucket, Region: d.Region, AccessKey: ak, SecretKey: sk}, nil
}

func (s *DBStore) SetBackupResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error {
	return s.q.SetBackupResult(ctx, db.SetBackupResultParams{
		ID: id, LastRunAt: pgtype.Timestamptz{Time: at, Valid: true}, LastStatus: status, LastError: errMsg,
	})
}

func (s *DBStore) ListEnabledBackups(ctx context.Context) ([]SchedBackup, error) {
	rows, err := s.q.ListEnabledBackups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SchedBackup, 0, len(rows))
	for _, b := range rows {
		out = append(out, SchedBackup{ID: b.ID, Schedule: b.Schedule})
	}
	return out, nil
}
