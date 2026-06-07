package notify

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory notify.Store for unit tests.
type fakeStore struct {
	channels map[int64][]Channel
	targets  map[int64]Target // appID -> target
	backups  map[int64]Target // backupID -> target
	watched  []WatchedApp
	health   int
}

func (f *fakeStore) ChannelsForOrg(_ context.Context, orgID int64) ([]Channel, error) {
	return f.channels[orgID], nil
}
func (f *fakeStore) AppTarget(_ context.Context, appID int64) (Target, error) {
	return f.targets[appID], nil
}
func (f *fakeStore) BackupTarget(_ context.Context, id int64) (Target, error) {
	return f.backups[id], nil
}
func (f *fakeStore) ListWatchedApps(_ context.Context) ([]WatchedApp, error) { return f.watched, nil }
func (f *fakeStore) EnabledHealthChannels(_ context.Context) (int, error)    { return f.health, nil }

type capturedSend struct {
	mu   sync.Mutex
	msgs []string
}

func (c *capturedSend) fn(_ context.Context, _, chatID, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, chatID+"|"+text)
	return nil
}
func (c *capturedSend) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.msgs) }

func TestNotifyRespectsCategoryToggle(t *testing.T) {
	st := &fakeStore{channels: map[int64][]Channel{
		7: {{Enabled: true, BotToken: "t", ChatID: "1", NotifyDeploy: false, NotifyBackup: true, NotifyHealth: true}},
	}}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn

	svc.notify(context.Background(), Event{Kind: DeployFailed, OrgID: 7, Time: time.Unix(0, 0)})
	if cap.count() != 0 {
		t.Fatalf("deploy-off channel should not send, got %d", cap.count())
	}
	svc.notify(context.Background(), Event{Kind: BackupFailed, OrgID: 7, Time: time.Unix(0, 0)})
	if cap.count() != 1 {
		t.Fatalf("backup-on channel should send once, got %d", cap.count())
	}
}

func TestNotifyDisabledChannelSilent(t *testing.T) {
	st := &fakeStore{channels: map[int64][]Channel{
		7: {{Enabled: false, NotifyDeploy: true, ChatID: "1"}},
	}}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	svc.notify(context.Background(), Event{Kind: DeployFailed, OrgID: 7, Time: time.Unix(0, 0)})
	if cap.count() != 0 {
		t.Fatalf("disabled channel must be silent, got %d", cap.count())
	}
}

func TestAppEventResolvesTarget(t *testing.T) {
	st := &fakeStore{targets: map[int64]Target{5: {OrgID: 7, Project: "p", Env: "e", Name: "web"}}}
	svc := New(st)
	ev, ok := svc.appEvent(context.Background(), DeployFailed, 5, "boom")
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.OrgID != 7 || ev.Project != "p" || ev.Env != "e" || ev.Target != "web" || ev.Detail != "boom" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestBackupEventResolvesTarget(t *testing.T) {
	st := &fakeStore{backups: map[int64]Target{9: {OrgID: 7, Project: "p", Env: "e", Name: "db1"}}}
	svc := New(st)
	ev, ok := svc.backupEvent(context.Background(), 9, "pg_dump failed")
	if !ok || ev.Kind != BackupFailed || ev.Target != "db1" {
		t.Fatalf("event = %+v ok=%v", ev, ok)
	}
	// OrgID drives channel lookup — a corrupted value would silently misroute.
	if ev.OrgID != 7 || ev.Project != "p" || ev.Env != "e" {
		t.Fatalf("target fields wrong: %+v", ev)
	}
}
