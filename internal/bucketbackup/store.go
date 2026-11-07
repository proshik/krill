package bucketbackup

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/secret"
)

// DBStore is the Store over the database.
type DBStore struct{ q *db.Queries }

// NewDBStore builds a DBStore.
func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) GetBackup(ctx context.Context, id int64) (Row, error) {
	b, err := s.q.GetBucketBackup(ctx, id)
	if err != nil {
		return Row{}, err
	}
	return Row{ID: b.ID, BucketID: b.BucketID, DestinationID: b.DestinationID, Prefix: b.Prefix, Retention: int(b.Retention)}, nil
}

// GetTarget resolves a bucket to its server and the server's root credentials.
func (s *DBStore) GetTarget(ctx context.Context, bucketID int64) (Target, error) {
	b, err := s.q.GetS3Bucket(ctx, bucketID)
	if err != nil {
		return Target{}, err
	}
	inst, err := s.q.GetDBInstance(ctx, b.InstanceID)
	if err != nil {
		return Target{}, err
	}
	pw, err := secret.Dec(inst.SuperuserPassword)
	if err != nil {
		return Target{}, fmt.Errorf("server %d root password: %w", inst.ID, err)
	}
	return Target{Server: inst.AppName, Bucket: b.Name, RootUser: inst.Superuser, RootPass: pw}, nil
}

func (s *DBStore) GetDestination(ctx context.Context, id int64) (backup.Destination, error) {
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

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func (s *DBStore) SetBackupResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error {
	return s.q.SetBucketBackupResult(ctx, db.SetBucketBackupResultParams{ID: id, LastRunAt: ts(at), LastStatus: status, LastError: errMsg})
}

func (s *DBStore) SetRestoreResult(ctx context.Context, id int64, at time.Time, status, errMsg string) error {
	return s.q.SetBucketRestoreResult(ctx, db.SetBucketRestoreResultParams{ID: id, LastRestoreAt: ts(at), LastRestoreStatus: status, LastRestoreError: errMsg})
}

// ListEnabledBackups feeds the scheduler.
func (s *DBStore) ListEnabledBackups(ctx context.Context) ([]backup.SchedBackup, error) {
	rows, err := s.q.ListEnabledBucketBackups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backup.SchedBackup, 0, len(rows))
	for _, r := range rows {
		out = append(out, backup.SchedBackup{ID: r.ID, Schedule: r.Schedule})
	}
	return out, nil
}

// MCIO is BucketIO through mc inside the S3 server's own container.
type MCIO struct{ Svc *dbservice.Service }

func (m MCIO) inst(t Target) dbservice.Instance {
	return dbservice.Instance{AppName: t.Server, Superuser: t.RootUser, SuperuserPassword: t.RootPass}
}

func (m MCIO) List(ctx context.Context, t Target) ([]Entry, error) {
	objs, err := m.Svc.ListObjects(ctx, m.inst(t), t.Bucket)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(objs))
	for i, o := range objs {
		out[i] = Entry{Key: o.Key, Size: o.Size}
	}
	return out, nil
}

func (m MCIO) Read(ctx context.Context, t Target, key string, w io.Writer) error {
	return m.Svc.ReadObject(ctx, m.inst(t), t.Bucket, key, w)
}

func (m MCIO) Write(ctx context.Context, t Target, key string, r io.Reader) error {
	return m.Svc.WriteObject(ctx, m.inst(t), t.Bucket, key, r)
}
