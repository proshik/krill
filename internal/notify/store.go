package notify

import (
	"context"
	"fmt"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

// Channel is a resolved (token-decrypted) notification channel.
type Channel struct {
	Type         string
	Enabled      bool
	BotToken     string
	ChatID       string
	NotifyDeploy bool
	NotifyBackup bool
	NotifyHealth bool
}

// wants reports whether this channel should receive the given event kind.
func (c Channel) wants(k EventKind) bool {
	switch k {
	case DeployFailed, MigrateFailed:
		return c.NotifyDeploy
	case BackupFailed:
		return c.NotifyBackup
	case AppDown, AppRecovered:
		return c.NotifyHealth
	}
	return false
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
	DBInstanceTarget(ctx context.Context, id int64) (Target, error)
	ListWatchedApps(ctx context.Context) ([]WatchedApp, error)
	EnabledHealthChannels(ctx context.Context) (int, error)
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
		tok, derr := secret.Dec(r.BotToken)
		if derr != nil {
			return nil, fmt.Errorf("org %d %s channel token: %w", orgID, r.Type, derr)
		}
		out = append(out, Channel{
			Type: r.Type, Enabled: r.Enabled,
			BotToken: tok, ChatID: r.ChatID,
			NotifyDeploy: r.NotifyDeploy, NotifyBackup: r.NotifyBackup, NotifyHealth: r.NotifyHealth,
		})
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
