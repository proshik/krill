package statebackup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

// Trigger is what started a run.
type Trigger string

const (
	TriggerSchedule  Trigger = "schedule"
	TriggerManual    Trigger = "manual"
	TriggerPreUpdate Trigger = "pre_update"
)

// Run is one finished backup attempt.
type Run struct {
	ID                            int64
	Trigger                       Trigger
	Status                        string // "ok" | "error"
	ObjectKey                     string
	SizeBytes                     int64
	FromVersion, ToVersion, Error string
	StartedAt, FinishedAt         time.Time
}

// Store persists the settings and the runs journal.
type Store interface {
	Load(ctx context.Context) (Settings, error)     // decrypts SecretKey; secret.ErrUndecryptable surfaces
	LoadView(ctx context.Context) (Settings, error) // never decrypts
	// Save stores s. SecretKey is encrypted with secret.Enc; an empty
	// SecretKey with HasSecretKey set keeps the stored (still encrypted) value.
	Save(ctx context.Context, s Settings) error
	SetEnabled(ctx context.Context, on bool) error
	RecordRun(ctx context.Context, r Run) error
	ListRuns(ctx context.Context, limit int) ([]Run, error)    // newest first
	RunsSince(ctx context.Context, t time.Time) ([]Run, error) // oldest first
	LastSuccess(ctx context.Context) (Run, bool, error)
	PruneRuns(ctx context.Context, before time.Time) error
}

// ErrNotSaved is returned by SetEnabled before any settings were saved.
var ErrNotSaved = errors.New("statebackup: the settings have not been saved yet")

// DBStore is the Postgres-backed Store.
type DBStore struct{ q *db.Queries }

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func fromRow(r db.StateBackupSetting) Settings {
	return Settings{
		Enabled:  r.Enabled,
		Endpoint: r.Endpoint, Bucket: r.Bucket, Region: r.Region, AccessKey: r.AccessKey,
		HasSecretKey: r.SecretKey != "",
		Prefix:       r.Prefix, Recipient: r.Recipient, Schedule: r.Schedule,
		Retention: int(r.Retention), UpdatedAt: r.UpdatedAt,
	}
}

func (s *DBStore) row(ctx context.Context) (Settings, string, error) {
	r, err := s.q.GetStateBackupSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{Retention: DefaultRetention}, "", nil
	}
	if err != nil {
		return Settings{}, "", err
	}
	return fromRow(r), r.SecretKey, nil
}

func (s *DBStore) LoadView(ctx context.Context) (Settings, error) {
	v, _, err := s.row(ctx)
	return v, err
}

func (s *DBStore) Load(ctx context.Context) (Settings, error) {
	v, raw, err := s.row(ctx)
	if err != nil {
		return Settings{}, err
	}
	if raw != "" {
		if v.SecretKey, err = secret.Dec(raw); err != nil {
			return Settings{}, fmt.Errorf("secret key: %w", err)
		}
	}
	return v, nil
}

func (s *DBStore) Save(ctx context.Context, v Settings) error {
	var enc string
	switch {
	case v.SecretKey != "":
		enc = secret.Enc(v.SecretKey)
	case v.HasSecretKey:
		// Keep the stored value as it is, even one that no longer decrypts.
		_, raw, err := s.row(ctx)
		if err != nil {
			return err
		}
		enc = raw
	}
	retention := v.Retention
	if retention == 0 {
		retention = DefaultRetention
	}
	// v.Enabled is not written: only SetEnabled changes the flag.
	return s.q.UpsertStateBackupSettings(ctx, db.UpsertStateBackupSettingsParams{
		Endpoint: v.Endpoint, Bucket: v.Bucket, Region: v.Region, AccessKey: v.AccessKey,
		SecretKey: enc, Prefix: v.Prefix, Recipient: v.Recipient, Schedule: v.Schedule,
		Retention: int32(retention),
	})
}

func (s *DBStore) SetEnabled(ctx context.Context, on bool) error {
	n, err := s.q.SetStateBackupEnabled(ctx, on)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotSaved
	}
	return nil
}

func (s *DBStore) RecordRun(ctx context.Context, r Run) error {
	fin := r.FinishedAt
	if fin.IsZero() {
		fin = time.Now()
	}
	return s.q.InsertStateBackupRun(ctx, db.InsertStateBackupRunParams{
		Trigger: string(r.Trigger), Status: r.Status, ObjectKey: r.ObjectKey, SizeBytes: r.SizeBytes,
		FromVersion: r.FromVersion, ToVersion: r.ToVersion, Error: r.Error,
		StartedAt: r.StartedAt, FinishedAt: fin,
	})
}

func runFromRow(r db.StateBackupRun) Run {
	return Run{
		ID: r.ID, Trigger: Trigger(r.Trigger), Status: r.Status, ObjectKey: r.ObjectKey,
		SizeBytes: r.SizeBytes, FromVersion: r.FromVersion, ToVersion: r.ToVersion, Error: r.Error,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

func runsFromRows(rows []db.StateBackupRun) []Run {
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		out = append(out, runFromRow(r))
	}
	return out
}

func (s *DBStore) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.q.ListStateBackupRuns(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	return runsFromRows(rows), nil
}

func (s *DBStore) RunsSince(ctx context.Context, t time.Time) ([]Run, error) {
	rows, err := s.q.ListStateBackupRunsSince(ctx, t)
	if err != nil {
		return nil, err
	}
	return runsFromRows(rows), nil
}

func (s *DBStore) LastSuccess(ctx context.Context) (Run, bool, error) {
	r, err := s.q.LastSuccessfulStateBackupRun(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return runFromRow(r), true, nil
}

func (s *DBStore) PruneRuns(ctx context.Context, before time.Time) error {
	return s.q.PruneStateBackupRuns(ctx, before)
}
