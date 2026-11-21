// Package notify sends operational alerts (deploy/backup failures, app health
// changes, successful deploys, node disk and memory thresholds) and a daily
// digest to configured channels. v1 channel: Telegram. Best-effort: a failed
// send is logged, never blocks or fails the source operation. The bot token is
// encrypted at rest and never logged.
package notify

import (
	"context"
	"log/slog"
	"sync/atomic"
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
	DeploySucceeded
	DiskHigh
	DiskRecovered
	MemOvercommit
	StateBackupFailed
	DBDown
	DBRecovered
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

	// Instance names the Krill installation (node-level messages: two
	// installations both have a "control-plane").
	Instance string

	Deploy *DeployDetail // DeploySucceeded
	Disk   *DiskDetail   // DiskHigh, DiskRecovered
	Mem    *MemDetail    // MemOvercommit
	State  *StateDetail  // StateBackupFailed
}

// DeployDetail is what a success message says about the deploy.
type DeployDetail struct {
	Image    string
	Trigger  string
	Duration time.Duration
}

// DiskDetail is one node filesystem crossing its threshold.
type DiskDetail struct {
	Node      string
	Mount     string
	Pct       float64
	Used      int64
	Total     int64
	Avail     int64 // free for unprivileged users, as df's Avail
	Threshold int
}

// MemDetail is a node whose containers' memory limits add up past its
// threshold; App is the deploy that prompted the check.
type MemDetail struct {
	Node      string
	App       string
	Limits    int64
	Total     int64
	Pct       float64
	Threshold int
	Unlimited int // containers without a memory limit (not in Limits)
}

// sendFunc sends one message; overridable in tests.
type sendFunc func(ctx context.Context, token, chatID, text string) error

// Service resolves events to channels and sends them. Best-effort.
type Service struct {
	store Store
	send  sendFunc
	log   *slog.Logger
	now   func() time.Time
	// monitor, when set, is asked to check a node's memory limits after every
	// successful deploy.
	monitor *ResourceMonitor
	// instanceName names this installation in node-level messages; set
	// after the watchers may already run, hence atomic.
	instanceName atomic.Pointer[func(ctx context.Context) string]
	// proxyVia / proxyErr record the Telegram proxy setting for the settings
	// page (written once by SetTelegramProxy at startup, before any reader).
	proxyVia string
	proxyErr error
}

// SetInstanceName sets how node-level messages and the digest name this
// installation (e.g. the panel's host). Safe to call at any time.
func (s *Service) SetInstanceName(f func(ctx context.Context) string) { s.instanceName.Store(&f) }

func (s *Service) instance(ctx context.Context) string {
	f := s.instanceName.Load()
	if f == nil || *f == nil {
		return ""
	}
	return (*f)(ctx)
}

// SetMonitor wires the resource monitor that checks memory overcommit after
// a successful deploy. Call before deploys start.
func (s *Service) SetMonitor(m *ResourceMonitor) { s.monitor = m }

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
	for _, c := range chans {
		if !c.Enabled || !c.wants(ev.Kind) {
			continue
		}
		_ = s.sendTo(ctx, c, ev)
	}
}

// sendTo sends one event to one channel in the channel's language.
func (s *Service) sendTo(ctx context.Context, c Channel, ev Event) error {
	err := s.send(ctx, c.BotToken, c.ChatID, format(ev, c.Lang))
	if err != nil {
		s.log.Warn("notify: send failed", "kind", ev.Kind, "channel", c.ID, "err", err)
	}
	return err
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

// backupFailedEvent resolves a failed backup's owner through lookup (each
// kind of backup has its own table and id sequence, so an id is only
// meaningful with the lookup of its own kind), records the run in the
// journal, and returns the alert to send.
func (s *Service) backupFailedEvent(ctx context.Context, kind string, lookup func(context.Context, int64) (Target, error), backupID int64, detail string) (Event, bool) {
	return s.backupAlert(ctx, kind, lookup, backupID, detail, true)
}

// backupAlert resolves the owner and builds the failure alert; record adds
// the run to the journal (a restore is not a backup run and is not recorded).
func (s *Service) backupAlert(ctx context.Context, kind string, lookup func(context.Context, int64) (Target, error), backupID int64, detail string, record bool) (Event, bool) {
	tgt, err := lookup(ctx, backupID)
	if err != nil {
		s.log.Warn("notify: backup target lookup failed", "backup", backupID, "kind", kind, "err", err)
		return Event{}, false
	}
	if record {
		s.recordRun(ctx, kind, backupID, tgt, "error", 0, detail)
	}
	return Event{Kind: BackupFailed, OrgID: tgt.OrgID, Project: tgt.Project, Env: tgt.Env, Target: tgt.Name, Detail: detail, Time: s.now()}, true
}

// backupSucceeded records a successful run in the journal. Successful
// backups are reported only in the daily digest (they run at night).
func (s *Service) backupSucceeded(kind string, lookup func(context.Context, int64) (Target, error), backupID, size int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		tgt, err := lookup(ctx, backupID)
		if err != nil {
			s.log.Warn("notify: backup target lookup failed", "backup", backupID, "kind", kind, "err", err)
			return
		}
		s.recordRun(ctx, kind, backupID, tgt, "ok", size, "")
	}()
}

func (s *Service) recordRun(ctx context.Context, kind string, id int64, tgt Target, status string, size int64, detail string) {
	if err := s.store.RecordBackupRun(ctx, BackupRun{
		OrgID: tgt.OrgID, Kind: kind, SourceID: id,
		Project: tgt.Project, Env: tgt.Env, Target: tgt.Name,
		Status: status, SizeBytes: size, Error: detail, FinishedAt: s.now(),
	}); err != nil {
		s.log.Warn("notify: record backup run failed", "backup", id, "kind", kind, "err", err)
	}
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

// DeploySucceeded implements deploy.Notifier: a deploy finished with the app
// running. Deploys the control plane makes itself (the organization network
// migration redeploys every app after an upgrade) are not reported.
func (s *Service) DeploySucceeded(_ context.Context, deploymentID int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		d, err := s.store.DeploymentInfo(ctx, deploymentID)
		if err != nil {
			s.log.Warn("notify: deployment lookup failed", "deployment", deploymentID, "err", err)
			return
		}
		if d.Trigger == triggerSystem {
			return
		}
		tgt := Target{OrgID: d.OrgID, Project: d.Project, Env: d.Env, Name: d.App}
		if s.monitor != nil {
			s.monitor.QueueOvercommitCheck(d.AppID, tgt, s.now())
		}
		var dur time.Duration
		if !d.FinishedAt.IsZero() && d.FinishedAt.After(d.StartedAt) {
			dur = d.FinishedAt.Sub(d.StartedAt)
		}
		s.notify(ctx, Event{
			Kind: DeploySucceeded, OrgID: d.OrgID, Project: d.Project, Env: d.Env, Target: d.App, Time: s.now(),
			Deploy: &DeployDetail{Image: shortImage(d.ImageTag), Trigger: d.Trigger, Duration: dur},
		})
	}()
}

// triggerSystem is deploy.TriggerSystem (not imported: deploy imports this
// package's consumer interface, not the other way round).
const triggerSystem = "system"

// BackupFailed implements backup.Notifier.
func (s *Service) BackupFailed(_ context.Context, backupID int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) {
		return s.backupFailedEvent(ctx, BackupKindDatabase, s.store.BackupTarget, backupID, reason)
	})
}

// BackupSucceeded implements backup.Notifier: size is the stored dump's size.
func (s *Service) BackupSucceeded(_ context.Context, backupID, size int64) {
	s.backupSucceeded(BackupKindDatabase, s.store.BackupTarget, backupID, size)
}

// VolumeBackupFailed is BackupFailed for a volume backup (a volume_backups id).
func (s *Service) VolumeBackupFailed(_ context.Context, id int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) {
		return s.backupFailedEvent(ctx, BackupKindVolume, s.store.VolumeBackupTarget, id, reason)
	})
}

// VolumeRestoreFailed alerts on a failed volume restore (a volume_backups
// id) on the backup toggle, without recording a backup run.
func (s *Service) VolumeRestoreFailed(_ context.Context, id int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) {
		return s.backupAlert(ctx, BackupKindVolume, s.store.VolumeBackupTarget, id, reason, false)
	})
}

// VolumeBackupSucceeded is BackupSucceeded for a volume backup.
func (s *Service) VolumeBackupSucceeded(_ context.Context, id, size int64) {
	s.backupSucceeded(BackupKindVolume, s.store.VolumeBackupTarget, id, size)
}

// BucketBackupFailed is BackupFailed for a bucket backup or restore (a
// bucket_backups id).
func (s *Service) BucketBackupFailed(_ context.Context, id int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) {
		return s.backupFailedEvent(ctx, BackupKindBucket, s.store.BucketBackupTarget, id, reason)
	})
}

// BucketRestoreFailed alerts on a failed bucket restore (a bucket_backups
// id) on the backup toggle, without recording a backup run.
func (s *Service) BucketRestoreFailed(_ context.Context, id int64, reason string) {
	s.async(func(ctx context.Context) (Event, bool) {
		return s.backupAlert(ctx, BackupKindBucket, s.store.BucketBackupTarget, id, reason, false)
	})
}

// BucketBackupSucceeded is BackupSucceeded for a bucket backup.
func (s *Service) BucketBackupSucceeded(_ context.Context, id, size int64) {
	s.backupSucceeded(BackupKindBucket, s.store.BucketBackupTarget, id, size)
}

// AppDown is emitted by the health watcher when an app becomes unreachable.
func (s *Service) AppDown(_ context.Context, appID int64, detail string) {
	s.async(func(ctx context.Context) (Event, bool) { return s.appEvent(ctx, AppDown, appID, detail) })
}

// AppRecovered is emitted by the health watcher when an app becomes reachable again.
func (s *Service) AppRecovered(_ context.Context, appID int64) {
	s.async(func(ctx context.Context) (Event, bool) { return s.appEvent(ctx, AppRecovered, appID, "") })
}

// DBDown is emitted by the health watcher when a database server that was
// running has no running task any more.
func (s *Service) DBDown(_ context.Context, instanceID int64, detail string) {
	s.async(func(ctx context.Context) (Event, bool) { return s.dbEvent(ctx, DBDown, instanceID, detail) })
}

// DBRecovered is emitted by the health watcher when a database server it
// reported down is running again.
func (s *Service) DBRecovered(_ context.Context, instanceID int64) {
	s.async(func(ctx context.Context) (Event, bool) { return s.dbEvent(ctx, DBRecovered, instanceID, "") })
}

func (s *Service) dbEvent(ctx context.Context, kind EventKind, instanceID int64, detail string) (Event, bool) {
	tgt, err := s.store.DBInstanceTarget(ctx, instanceID)
	if err != nil {
		s.log.Warn("notify: db instance target lookup failed", "instance", instanceID, "err", err)
		return Event{}, false
	}
	return Event{Kind: kind, OrgID: tgt.OrgID, Target: tgt.Name, Detail: detail, Time: s.now()}, true
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
