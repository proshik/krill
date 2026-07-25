// Package notify sends operational alerts (deploy/backup failures, app health
// changes) to configured channels. v1 channel: Telegram. Best-effort: a failed
// send is logged, never blocks or fails the source operation. The bot token is
// encrypted at rest and never logged.
package notify

import (
	"context"
	"log/slog"
	"time"
)

// EventKind is the kind of operational event being reported.
type EventKind int

const (
	DeployFailed EventKind = iota
	BackupFailed
	AppDown
	AppRecovered
	MigrateFailed
)

// Event is a resolved, human-targetable description of something that happened.
type Event struct {
	Kind    EventKind
	OrgID   int64
	Project string
	Env     string
	Target  string // app or db name
	Detail  string // short reason/state; may be empty
	Time    time.Time
}

// sendFunc sends one message; overridable in tests.
type sendFunc func(ctx context.Context, token, chatID, text string) error

// Service resolves events to channels and sends them. Best-effort.
type Service struct {
	store Store
	send  sendFunc
	log   *slog.Logger
	now   func() time.Time
}

func New(store Store) *Service {
	return &Service{store: store, send: sendTelegram, log: slog.Default(), now: time.Now}
}

// notify loads the org's enabled channels and sends the formatted message to
// each one whose category toggle matches. Synchronous; callers run it async.
func (s *Service) notify(ctx context.Context, ev Event) {
	chans, err := s.store.ChannelsForOrg(ctx, ev.OrgID)
	if err != nil {
		s.log.Warn("notify: load channels failed", "org", ev.OrgID, "err", err)
		return
	}
	text := format(ev)
	for _, c := range chans {
		if !c.Enabled || !c.wants(ev.Kind) {
			continue
		}
		if err := s.send(ctx, c.BotToken, c.ChatID, text); err != nil {
			s.log.Warn("notify: send failed", "kind", ev.Kind, "err", err)
		}
	}
}

// SendTest sends an ad-hoc message synchronously and returns the result (used by
// the "Send test message" button). The token is never logged.
func (s *Service) SendTest(ctx context.Context, token, chatID, text string) error {
	return s.send(ctx, token, chatID, text)
}

// notifyTimeout bounds a single detached resolve+send.
const notifyTimeout = 15 * time.Second

func (s *Service) appEvent(ctx context.Context, kind EventKind, appID int64, detail string) (Event, bool) {
	tgt, err := s.store.AppTarget(ctx, appID)
	if err != nil {
		s.log.Warn("notify: app target lookup failed", "app", appID, "err", err)
		return Event{}, false
	}
	return Event{Kind: kind, OrgID: tgt.OrgID, Project: tgt.Project, Env: tgt.Env, Target: tgt.Name, Detail: detail, Time: s.now()}, true
}

func (s *Service) backupEvent(ctx context.Context, backupID int64, detail string) (Event, bool) {
	tgt, err := s.store.BackupTarget(ctx, backupID)
	if err != nil {
		s.log.Warn("notify: backup target lookup failed", "backup", backupID, "err", err)
		return Event{}, false
	}
	return Event{Kind: BackupFailed, OrgID: tgt.OrgID, Project: tgt.Project, Env: tgt.Env, Target: tgt.Name, Detail: detail, Time: s.now()}, true
}

// async resolves + sends on a detached, timeout-bound context so a canceled
// caller ctx (e.g. graceful shutdown) does not drop the notification.
func (s *Service) async(build func(ctx context.Context) (Event, bool)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		if ev, ok := build(ctx); ok {
			s.notify(ctx, ev)
		}
	}()
}

// The public notifier methods below intentionally ignore the caller's context:
// the work runs on a detached, timeout-bound context (see async) so a canceled
// caller ctx (deploy/backup finishing, graceful shutdown) never drops an alert.

// DeployFailed implements deploy.Notifier.
func (s *Service) DeployFailed(_ context.Context, appID int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) { return s.appEvent(ctx, DeployFailed, appID, reason) })
}

// BackupFailed implements backup.Notifier.
func (s *Service) BackupFailed(_ context.Context, backupID int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) { return s.backupEvent(ctx, backupID, reason) })
}

// AppDown is emitted by the health watcher when an app becomes unreachable.
func (s *Service) AppDown(_ context.Context, appID int64, detail string) {
	s.async(func(ctx context.Context) (Event, bool) { return s.appEvent(ctx, AppDown, appID, detail) })
}

// AppRecovered is emitted by the health watcher when an app becomes reachable again.
func (s *Service) AppRecovered(_ context.Context, appID int64) {
	s.async(func(ctx context.Context) (Event, bool) { return s.appEvent(ctx, AppRecovered, appID, "") })
}

// MigrateFailed implements dbservice.Notifier (volume-migration failures ride
// the deploy category toggle).
func (s *Service) MigrateFailed(_ context.Context, instanceID int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) {
		tgt, err := s.store.DBInstanceTarget(ctx, instanceID)
		if err != nil {
			s.log.Warn("notify: db instance target lookup failed", "instance", instanceID, "err", err)
			return Event{}, false
		}
		return Event{Kind: MigrateFailed, OrgID: tgt.OrgID, Target: tgt.Name, Detail: reason, Time: s.now()}, true
	})
}
