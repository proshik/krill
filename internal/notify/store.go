package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

// Channel is a resolved (token-decrypted) notification channel.
type Channel struct {
	ID           int64
	OrgID        int64
	Type         string
	Enabled      bool
	BotToken     string
	ChatID       string
	NotifyDeploy bool
	NotifyBackup bool
	NotifyHealth bool
	// NotifyDeploySuccess sends a message for every successful deploy.
	NotifyDeploySuccess bool
	// NotifyInfra receives node-level data (disk and memory alerts, the
	// digest's node section). Only an instance operator can turn it on: the
	// nodes are shared by every organization.
	NotifyInfra      bool
	DiskThresholdPct int
	MemThresholdPct  int
	DigestEnabled    bool
	DigestMinute     int    // minutes after local midnight
	DigestTZ         string // IANA zone name
	DigestLastAt     *time.Time
	Lang             string // message language: "en" or "ru"
}

// wants reports whether this channel should receive the given event kind.
func (c Channel) wants(k EventKind) bool {
	switch k {
	case DeployFailed, MigrateFailed:
		return c.NotifyDeploy
	case DeploySucceeded:
		return c.NotifyDeploySuccess
	case BackupFailed:
		return c.NotifyBackup
	case AppDown, AppRecovered:
		return c.NotifyHealth
	case DiskHigh, DiskRecovered, MemOvercommit, StateBackupFailed:
		return c.NotifyInfra
	}
	return false
}

// ChannelFromRow decrypts a stored channel.
func ChannelFromRow(r db.NotificationChannel) (Channel, error) {
	tok, err := secret.Dec(r.BotToken)
	if err != nil {
		return Channel{}, fmt.Errorf("org %d %s channel token: %w", r.OrgID, r.Type, err)
	}
	c := Channel{
		ID: r.ID, OrgID: r.OrgID, Type: r.Type, Enabled: r.Enabled,
		BotToken: tok, ChatID: r.ChatID,
		NotifyDeploy: r.NotifyDeploy, NotifyBackup: r.NotifyBackup, NotifyHealth: r.NotifyHealth,
		NotifyDeploySuccess: r.NotifyDeploySuccess, NotifyInfra: r.NotifyInfra,
		DiskThresholdPct: int(r.DiskThresholdPct), MemThresholdPct: int(r.MemThresholdPct),
		DigestEnabled: r.DigestEnabled, DigestMinute: int(r.DigestMinute), DigestTZ: r.DigestTz,
		Lang: r.Lang,
	}
	if r.DigestLastAt.Valid {
		t := r.DigestLastAt.Time
		c.DigestLastAt = &t
	}
	return c, nil
}

// AlertState is a channel's memory of one threshold alert.
type AlertState struct {
	Active   bool
	LastSent *time.Time
}

// BackupRun is one finished backup run, as the digest reports it.
type BackupRun struct {
	OrgID      int64
	Kind       string // BackupKindDatabase/Volume/Bucket
	SourceID   int64
	Project    string
	Env        string
	Target     string
	Status     string // "ok" or "error"
	SizeBytes  int64
	Error      string
	FinishedAt time.Time
}

// Backup run kinds (backup_runs.kind).
const (
	BackupKindDatabase = "database"
	BackupKindVolume   = "volume"
	BackupKindBucket   = "bucket"
)

// DeploymentInfo describes one deployment for a message or the digest.
type DeploymentInfo struct {
	ID         int64
	AppID      int64
	OrgID      int64
	Project    string
	Env        string
	App        string
	Status     string // deployments.status: running/done/error
	Trigger    string
	ImageTag   string
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Target is the human-readable location of an event's subject.
type Target struct {
	OrgID   int64
	Project string
	Env     string
	Name    string
}

// WatchedApp is one app the health watcher tracks.
type WatchedApp struct {
	AppID   int64
	OrgID   int64
	Project string
	Env     string
	Name    string
}

// Store is the read-only persistence the notify package needs.
type Store interface {
	ChannelsForOrg(ctx context.Context, orgID int64) ([]Channel, error)
	AppTarget(ctx context.Context, appID int64) (Target, error)
	BackupTarget(ctx context.Context, backupID int64) (Target, error)
	VolumeBackupTarget(ctx context.Context, id int64) (Target, error)
	BucketBackupTarget(ctx context.Context, id int64) (Target, error)
	DBInstanceTarget(ctx context.Context, id int64) (Target, error)
	ListWatchedApps(ctx context.Context) ([]WatchedApp, error)
	EnabledHealthChannels(ctx context.Context) (int, error)

	// InfraChannels lists every organization's enabled channel with node-level
	// alerts on; DigestChannels every enabled channel with the digest on.
	InfraChannels(ctx context.Context) ([]Channel, error)
	DigestChannels(ctx context.Context) ([]Channel, error)
	// ClaimDigest records the digest scheduled at `scheduled` as sent at `at`,
	// reporting false when it was already claimed. ReleaseDigest undoes the
	// claim made at `at`, restoring `previous`.
	ClaimDigest(ctx context.Context, channelID int64, at, scheduled time.Time) (bool, error)
	ReleaseDigest(ctx context.Context, channelID int64, at time.Time, previous *time.Time) error

	AlertState(ctx context.Context, channelID int64, key string) (AlertState, error)
	SetAlertState(ctx context.Context, channelID int64, key string, st AlertState) error

	RecordBackupRun(ctx context.Context, r BackupRun) error
	BackupRunsSince(ctx context.Context, orgID int64, since time.Time) ([]BackupRun, error)
	PruneBackupRuns(ctx context.Context, before time.Time) error

	DeploymentInfo(ctx context.Context, deploymentID int64) (DeploymentInfo, error)
	OrgDeploymentsSince(ctx context.Context, orgID int64, since time.Time) ([]DeploymentInfo, error)
	OrgApps(ctx context.Context, orgID int64) ([]WatchedApp, error)
}

// DBStore implements Store over sqlc.
type DBStore struct{ q *db.Queries }

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) ChannelsForOrg(ctx context.Context, orgID int64) ([]Channel, error) {
	rows, err := s.q.ChannelsForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(rows))
	for _, r := range rows {
		c, err := ChannelFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// channelsSkippingBroken decrypts rows from several organizations. One
// channel with an undecryptable token must not silence every other
// organization's, so it is reported in the error but skipped.
func channelsSkippingBroken(rows []db.NotificationChannel) ([]Channel, error) {
	out := make([]Channel, 0, len(rows))
	var errs []error
	for _, r := range rows {
		c, err := ChannelFromRow(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, c)
	}
	return out, errors.Join(errs...)
}

func (s *DBStore) InfraChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.q.InfraChannels(ctx)
	if err != nil {
		return nil, err
	}
	return channelsSkippingBroken(rows)
}

func (s *DBStore) DigestChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.q.DigestChannels(ctx)
	if err != nil {
		return nil, err
	}
	return channelsSkippingBroken(rows)
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func (s *DBStore) ClaimDigest(ctx context.Context, channelID int64, at, scheduled time.Time) (bool, error) {
	n, err := s.q.ClaimDigest(ctx, db.ClaimDigestParams{ID: channelID, ClaimedAt: at, ScheduledAt: scheduled})
	return n == 1, err
}

func (s *DBStore) ReleaseDigest(ctx context.Context, channelID int64, at time.Time, previous *time.Time) error {
	prev := pgtype.Timestamptz{}
	if previous != nil {
		prev = ts(*previous)
	}
	return s.q.ReleaseDigest(ctx, db.ReleaseDigestParams{ID: channelID, ClaimedAt: at, Previous: prev})
}

func (s *DBStore) AlertState(ctx context.Context, channelID int64, key string) (AlertState, error) {
	r, err := s.q.GetAlertState(ctx, db.GetAlertStateParams{ChannelID: channelID, AlertKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertState{}, nil
	}
	if err != nil {
		return AlertState{}, err
	}
	st := AlertState{Active: r.Active}
	if r.LastSentAt.Valid {
		t := r.LastSentAt.Time
		st.LastSent = &t
	}
	return st, nil
}

func (s *DBStore) SetAlertState(ctx context.Context, channelID int64, key string, st AlertState) error {
	last := pgtype.Timestamptz{}
	if st.LastSent != nil {
		last = ts(*st.LastSent)
	}
	return s.q.SetAlertState(ctx, db.SetAlertStateParams{ChannelID: channelID, AlertKey: key, Active: st.Active, LastSentAt: last})
}

func (s *DBStore) RecordBackupRun(ctx context.Context, r BackupRun) error {
	return s.q.InsertBackupRun(ctx, db.InsertBackupRunParams{
		OrgID: r.OrgID, Kind: r.Kind, SourceID: r.SourceID,
		Project: r.Project, Env: r.Env, Target: r.Target,
		Status: r.Status, SizeBytes: r.SizeBytes, Error: r.Error, FinishedAt: r.FinishedAt,
	})
}

func (s *DBStore) BackupRunsSince(ctx context.Context, orgID int64, since time.Time) ([]BackupRun, error) {
	rows, err := s.q.ListBackupRunsSince(ctx, db.ListBackupRunsSinceParams{OrgID: orgID, FinishedAt: since})
	if err != nil {
		return nil, err
	}
	out := make([]BackupRun, 0, len(rows))
	for _, r := range rows {
		out = append(out, BackupRun{
			OrgID: r.OrgID, Kind: r.Kind, SourceID: r.SourceID,
			Project: r.Project, Env: r.Env, Target: r.Target,
			Status: r.Status, SizeBytes: r.SizeBytes, Error: r.Error, FinishedAt: r.FinishedAt,
		})
	}
	return out, nil
}

func (s *DBStore) PruneBackupRuns(ctx context.Context, before time.Time) error {
	return s.q.PruneBackupRuns(ctx, before)
}

func (s *DBStore) DeploymentInfo(ctx context.Context, id int64) (DeploymentInfo, error) {
	r, err := s.q.DeploymentNotifyInfo(ctx, id)
	if err != nil {
		return DeploymentInfo{}, err
	}
	return DeploymentInfo{
		ID: r.ID, AppID: r.ApplicationID, OrgID: r.OrgID,
		Project: r.ProjectName, Env: r.EnvName, App: r.AppName,
		Status: r.Status, Trigger: r.Trigger, ImageTag: r.ImageTag,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt.Time,
	}, nil
}

func (s *DBStore) OrgDeploymentsSince(ctx context.Context, orgID int64, since time.Time) ([]DeploymentInfo, error) {
	rows, err := s.q.ListOrgDeploymentsSince(ctx, db.ListOrgDeploymentsSinceParams{OrganizationID: orgID, FinishedAt: ts(since)})
	if err != nil {
		return nil, err
	}
	out := make([]DeploymentInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, DeploymentInfo{
			ID: r.ID, AppID: r.ApplicationID, OrgID: orgID,
			Project: r.ProjectName, Env: r.EnvName, App: r.AppName,
			Status: r.Status, Trigger: r.Trigger, ImageTag: r.ImageTag, Error: r.ErrorMessage,
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt.Time,
		})
	}
	return out, nil
}

func (s *DBStore) OrgApps(ctx context.Context, orgID int64) ([]WatchedApp, error) {
	rows, err := s.q.ListOrgApps(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]WatchedApp, 0, len(rows))
	for _, r := range rows {
		out = append(out, WatchedApp{AppID: r.AppID, OrgID: orgID, Project: r.ProjectName, Env: r.EnvName, Name: r.AppName})
	}
	return out, nil
}

func (s *DBStore) AppTarget(ctx context.Context, appID int64) (Target, error) {
	r, err := s.q.AppNotifyTarget(ctx, appID)
	if err != nil {
		return Target{}, err
	}
	return Target{OrgID: r.OrgID, Project: r.ProjectName, Env: r.EnvName, Name: r.AppName}, nil
}

func (s *DBStore) BackupTarget(ctx context.Context, backupID int64) (Target, error) {
	r, err := s.q.BackupNotifyTarget(ctx, backupID)
	if err != nil {
		return Target{}, err
	}
	return Target{OrgID: r.OrgID, Project: r.ProjectName, Env: r.EnvName, Name: r.DbName}, nil
}

func (s *DBStore) VolumeBackupTarget(ctx context.Context, id int64) (Target, error) {
	r, err := s.q.VolumeBackupNotifyTarget(ctx, id)
	if err != nil {
		return Target{}, err
	}
	return Target{OrgID: r.OrgID, Project: r.ProjectName, Env: r.EnvName, Name: r.TargetName}, nil
}

func (s *DBStore) BucketBackupTarget(ctx context.Context, id int64) (Target, error) {
	r, err := s.q.BucketBackupNotifyTarget(ctx, id)
	if err != nil {
		return Target{}, err
	}
	return Target{OrgID: r.OrgID, Project: r.ProjectName, Env: r.EnvName, Name: r.TargetName}, nil
}

func (s *DBStore) DBInstanceTarget(ctx context.Context, id int64) (Target, error) {
	r, err := s.q.GetDBInstance(ctx, id)
	if err != nil {
		return Target{}, err
	}
	return Target{OrgID: r.OrganizationID, Name: r.Name}, nil
}

func (s *DBStore) ListWatchedApps(ctx context.Context) ([]WatchedApp, error) {
	rows, err := s.q.ListWatchedApps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WatchedApp, 0, len(rows))
	for _, r := range rows {
		out = append(out, WatchedApp{AppID: r.AppID, OrgID: r.OrgID, Project: r.ProjectName, Env: r.EnvName, Name: r.AppName})
	}
	return out, nil
}

func (s *DBStore) EnabledHealthChannels(ctx context.Context) (int, error) {
	n, err := s.q.CountEnabledHealthChannels(ctx)
	return int(n), err
}
