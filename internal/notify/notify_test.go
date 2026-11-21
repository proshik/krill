package notify

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory notify.Store for unit tests.
type fakeStore struct {
	channels  map[int64][]Channel
	targets   map[int64]Target // appID -> target
	backups   map[int64]Target // backupID -> target
	dbTargets map[int64]Target // instanceID -> target

	volumeBackups, bucketBackups map[int64]Target
	watched                      []WatchedApp
	health                       int

	mu          sync.Mutex
	infra       []Channel
	infraErr    error // returned alongside infra, like a skipped broken row
	digest      []Channel
	alerts      map[string]AlertState // "<channel>|<key>"
	runs        []BackupRun
	deployments map[int64]DeploymentInfo
	orgDeploys  []DeploymentInfo
	orgApps     []WatchedApp
	claims      map[int64]time.Time // channel → digest_last_at
	released    int
}

func (f *fakeStore) InfraChannels(context.Context) ([]Channel, error)  { return f.infra, f.infraErr }
func (f *fakeStore) DigestChannels(context.Context) ([]Channel, error) { return f.digest, nil }
func (f *fakeStore) ClaimDigest(_ context.Context, id int64, at, scheduled time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims == nil {
		f.claims = map[int64]time.Time{}
	}
	if last, ok := f.claims[id]; ok && !last.Before(scheduled) {
		return false, nil
	}
	f.claims[id] = at
	return true, nil
}
func (f *fakeStore) ReleaseDigest(_ context.Context, id int64, at time.Time, prev *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims[id].Equal(at) {
		if prev == nil {
			delete(f.claims, id)
		} else {
			f.claims[id] = *prev
		}
		f.released++
	}
	return nil
}
func (f *fakeStore) AlertState(_ context.Context, ch int64, key string) (AlertState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alerts[strconv.FormatInt(ch, 10)+"|"+key], nil
}
func (f *fakeStore) SetAlertState(_ context.Context, ch int64, key string, st AlertState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alerts == nil {
		f.alerts = map[string]AlertState{}
	}
	f.alerts[strconv.FormatInt(ch, 10)+"|"+key] = st
	return nil
}
func (f *fakeStore) RecordBackupRun(_ context.Context, r BackupRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, r)
	return nil
}
func (f *fakeStore) recordedRuns() []BackupRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BackupRun(nil), f.runs...)
}
func (f *fakeStore) BackupRunsSince(_ context.Context, org int64, since time.Time) ([]BackupRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []BackupRun
	for _, r := range f.runs {
		if r.OrgID == org && !r.FinishedAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeStore) PruneBackupRuns(context.Context, time.Time) error { return nil }
func (f *fakeStore) DeploymentInfo(_ context.Context, id int64) (DeploymentInfo, error) {
	return f.deployments[id], nil
}
func (f *fakeStore) OrgDeploymentsSince(context.Context, int64, time.Time) ([]DeploymentInfo, error) {
	return f.orgDeploys, nil
}
func (f *fakeStore) OrgApps(context.Context, int64) ([]WatchedApp, error) { return f.orgApps, nil }

func (f *fakeStore) ChannelsForOrg(_ context.Context, orgID int64) ([]Channel, error) {
	return f.channels[orgID], nil
}
func (f *fakeStore) AppTarget(_ context.Context, appID int64) (Target, error) {
	return f.targets[appID], nil
}
func (f *fakeStore) BackupTarget(_ context.Context, id int64) (Target, error) {
	return f.backups[id], nil
}
func (f *fakeStore) VolumeBackupTarget(_ context.Context, id int64) (Target, error) {
	return f.volumeBackups[id], nil
}
func (f *fakeStore) BucketBackupTarget(_ context.Context, id int64) (Target, error) {
	return f.bucketBackups[id], nil
}
func (f *fakeStore) DBInstanceTarget(_ context.Context, id int64) (Target, error) {
	return f.dbTargets[id], nil
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
	ev, ok := svc.backupFailedEvent(context.Background(), BackupKindDatabase, st.BackupTarget, 9, "pg_dump failed")
	if !ok || ev.Kind != BackupFailed || ev.Target != "db1" {
		t.Fatalf("event = %+v ok=%v", ev, ok)
	}
	// OrgID drives channel lookup — a corrupted value would silently misroute.
	if ev.OrgID != 7 || ev.Project != "p" || ev.Env != "e" {
		t.Fatalf("target fields wrong: %+v", ev)
	}
	// The failure also lands in the journal the digest reads.
	runs := st.recordedRuns()
	if len(runs) != 1 || runs[0].Status != "error" || runs[0].Kind != BackupKindDatabase || runs[0].OrgID != 7 || runs[0].Error != "pg_dump failed" {
		t.Fatalf("journal = %+v", runs)
	}
}

func TestChannelWantsMigrate(t *testing.T) {
	if !(Channel{NotifyDeploy: true}).wants(MigrateFailed) {
		t.Fatal("deploy-toggled channel must receive MigrateFailed")
	}
	if (Channel{NotifyBackup: true, NotifyHealth: true}).wants(MigrateFailed) {
		t.Fatal("non-deploy channel must not receive MigrateFailed")
	}
}
