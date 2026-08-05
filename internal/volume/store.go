package volume

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
)

// VolTarget is what a restore/backup needs to locate the docker volume + service.
type VolTarget struct {
	AppVolumeID int64
	AppID       int64
	VolumeName  string // logical name → docker.VolumeName(AppID, VolumeName)
	ServiceName string // docker.ServiceName(AppID)
	Replicas    uint64 // scale to restore back to (>=1)
}

// VolBackupRow is the minimal volume-backup config the service needs.
type VolBackupRow struct {
	ID            int64
	AppVolumeID   int64
	DestinationID int64
	Prefix        string
	Retention     int
}

// VolumeStore is what VolumeService needs from persistence. ListEnabledBackups
// is named to satisfy backup.SchedStore (so the generic scheduler can drive it).
type VolumeStore interface {
	GetVolumeBackup(ctx context.Context, id int64) (VolBackupRow, error)
	GetVolTarget(ctx context.Context, appVolumeID int64) (VolTarget, error)
	GetDestination(ctx context.Context, id int64) (backup.Destination, error)
	SetVolumeBackupResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error
	ListEnabledBackups(ctx context.Context) ([]backup.SchedBackup, error)
}

type DBVolumeStore struct{ q *db.Queries }

func NewDBStore(q *db.Queries) *DBVolumeStore { return &DBVolumeStore{q: q} }

func (s *DBVolumeStore) GetVolumeBackup(ctx context.Context, id int64) (VolBackupRow, error) {
	b, err := s.q.GetVolumeBackup(ctx, id)
	if err != nil {
		return VolBackupRow{}, err
	}
	return VolBackupRow{ID: b.ID, AppVolumeID: b.AppVolumeID, DestinationID: b.DestinationID, Prefix: b.Prefix, Retention: int(b.Retention)}, nil
}

func (s *DBVolumeStore) GetVolTarget(ctx context.Context, appVolumeID int64) (VolTarget, error) {
	t, err := s.q.GetVolTarget(ctx, appVolumeID)
	if err != nil {
		return VolTarget{}, err
	}
	rep := uint64(t.Replicas)
	if rep == 0 {
		rep = 1
	}
	return VolTarget{
		AppVolumeID: t.AppVolumeID, AppID: t.ApplicationID, VolumeName: t.VolumeName,
		ServiceName: docker.ServiceName(t.ApplicationID), Replicas: rep,
	}, nil
}

// GetDestination mirrors backup.DBStore.GetDestination — decrypts the S3 keys.
func (s *DBVolumeStore) GetDestination(ctx context.Context, id int64) (backup.Destination, error) {
	d, err := s.q.GetDestination(ctx, id)
	if err != nil {
		return backup.Destination{}, err
	}
	ak, err := secret.Dec(d.AccessKey)
	if err != nil {
		return backup.Destination{}, fmt.Errorf("destination %d access key: %w", d.ID, err)
	}
	sk, err := secret.Dec(d.SecretKey)
	if err != nil {
		return backup.Destination{}, fmt.Errorf("destination %d secret key: %w", d.ID, err)
	}
	return backup.Destination{Endpoint: d.Endpoint, Bucket: d.Bucket, Region: d.Region, AccessKey: ak, SecretKey: sk}, nil
}

func (s *DBVolumeStore) SetVolumeBackupResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error {
	return s.q.SetVolumeBackupResult(ctx, db.SetVolumeBackupResultParams{
		ID: id, LastRunAt: pgtype.Timestamptz{Time: at, Valid: true}, LastStatus: status, LastError: errMsg,
	})
}

// ListEnabledBackups satisfies backup.SchedStore (used by a 2nd Scheduler).
func (s *DBVolumeStore) ListEnabledBackups(ctx context.Context) ([]backup.SchedBackup, error) {
	rows, err := s.q.ListEnabledVolumeBackups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backup.SchedBackup, 0, len(rows))
	for _, b := range rows {
		out = append(out, backup.SchedBackup{ID: b.ID, Schedule: b.Schedule})
	}
	return out, nil
}
