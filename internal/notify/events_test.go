package notify

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
)

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *capturedSend) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func TestDeploySucceededGoesToSuccessChannelsInTheirLanguage(t *testing.T) {
	st := &fakeStore{
		channels: map[int64][]Channel{7: {
			{ID: 1, Enabled: true, ChatID: "ru", NotifyDeploySuccess: true, Lang: "ru"},
			{ID: 2, Enabled: true, ChatID: "off", NotifyDeploy: true}, // failures only
		}},
		deployments: map[int64]DeploymentInfo{3: {
			ID: 3, AppID: 5, OrgID: 7, Project: "p", Env: "prod", App: "bot", Trigger: "manual",
			ImageTag:  "ghcr.io/acme/bot:v1.2@sha256:0123456789abcdef0123",
			StartedAt: time.Unix(100, 0), FinishedAt: time.Unix(142, 0),
		}},
	}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	svc.DeploySucceeded(context.Background(), 3)
	waitUntil(t, func() bool { return cap.count() == 1 })
	time.Sleep(20 * time.Millisecond)
	msgs := cap.all()
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0], "ru|") {
		t.Fatalf("sent %v, want only the success channel", msgs)
	}
	for _, want := range []string{"Деплой успешен", "\nbot\nпроект p · окружение prod\n", "образ bot:v1.2 · 42 с · вручную"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("message lacks %q:\n%s", want, msgs[0])
		}
	}
	if strings.Contains(msgs[0], "sha256") {
		t.Errorf("digest not dropped next to a tag:\n%s", msgs[0])
	}
}

// The organization network migration redeploys every app after an upgrade;
// reporting each as a success would flood the chat.
func TestDeploySucceededSkipsSystemDeploys(t *testing.T) {
	st := &fakeStore{
		channels:    map[int64][]Channel{7: {{ID: 1, Enabled: true, ChatID: "c", NotifyDeploySuccess: true}}},
		deployments: map[int64]DeploymentInfo{3: {ID: 3, AppID: 5, OrgID: 7, Trigger: "system"}},
	}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	mon := NewResourceMonitor(svc, st)
	svc.SetMonitor(mon)
	svc.DeploySucceeded(context.Background(), 3)
	time.Sleep(50 * time.Millisecond)
	if cap.count() != 0 {
		t.Fatalf("system deploy reported: %v", cap.all())
	}
	mon.mu.Lock()
	defer mon.mu.Unlock()
	if len(mon.pending) != 0 {
		t.Fatalf("system deploy queued a memory check: %v", mon.pending)
	}
}

func TestDeploySucceededQueuesOvercommitCheck(t *testing.T) {
	st := &fakeStore{deployments: map[int64]DeploymentInfo{3: {ID: 3, AppID: 5, OrgID: 7, Project: "p", Env: "e", App: "a", Trigger: "api"}}}
	svc := New(st)
	svc.send = (&capturedSend{}).fn
	mon := NewResourceMonitor(svc, st)
	svc.SetMonitor(mon)
	svc.DeploySucceeded(context.Background(), 3)
	waitUntil(t, func() bool {
		mon.mu.Lock()
		defer mon.mu.Unlock()
		return mon.pending[5].target.Name == "a"
	})
}

func TestBackupSucceededRecordsRunWithoutMessage(t *testing.T) {
	st := &fakeStore{
		channels:      map[int64][]Channel{7: {{ID: 1, Enabled: true, ChatID: "c", NotifyBackup: true, NotifyDeploySuccess: true}}},
		volumeBackups: map[int64]Target{4: {OrgID: 7, Project: "p", Env: "e", Name: "web / data"}},
	}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	svc.VolumeBackupSucceeded(context.Background(), 4, 2048)
	waitUntil(t, func() bool { return len(st.recordedRuns()) == 1 })
	r := st.recordedRuns()[0]
	if r.Kind != BackupKindVolume || r.Status != "ok" || r.SizeBytes != 2048 || r.OrgID != 7 || r.Target != "web / data" || r.SourceID != 4 {
		t.Fatalf("journal row = %+v", r)
	}
	time.Sleep(20 * time.Millisecond)
	if cap.count() != 0 {
		t.Fatalf("a successful backup sent a message (they run at night): %v", cap.all())
	}
}

func TestFormatInRussianAndUnknownLanguage(t *testing.T) {
	ev := Event{Kind: DeployFailed, Project: "p", Env: "e", Target: "w", Detail: "boom"}
	if got := format(ev, "ru"); !strings.Contains(got, "Деплой упал") || !strings.Contains(got, "\nw\nпроект p · окружение e") {
		t.Fatalf("ru: %s", got)
	}
	if got := format(ev, "xx"); !strings.Contains(got, "Deploy failed") {
		t.Fatalf("unknown language must fall back to English: %s", got)
	}
}

func TestFormatNodeAlerts(t *testing.T) {
	disk := format(Event{Kind: DiskHigh, Instance: "krill.example.com", Disk: &DiskDetail{
		Node: "control-plane", Mount: "/", Pct: 87.2, Used: 43 << 30, Total: 50 << 30, Avail: 6 << 30, Threshold: 85,
	}}, "en")
	for _, want := range []string{"Disk almost full · krill.example.com", "node control-plane, disk /: 87% used (43.0 of 50.0 GiB), 6.0 GiB free, threshold 85%"} {
		if !strings.Contains(disk, want) {
			t.Errorf("disk alert lacks %q:\n%s", want, disk)
		}
	}
	mem := format(Event{Kind: MemOvercommit, Mem: &MemDetail{
		Node: "worker-1", App: "p/e/a", Limits: 2 << 30, Total: 1 << 30, Pct: 200, Threshold: 100, Unlimited: 2,
	}}, "ru")
	for _, want := range []string{"Переподписка памяти", "нода worker-1: сумма лимитов памяти 2.0 GiB — 200% памяти ноды (1.0 GiB), порог 100%", "после деплоя p/e/a", "без лимита памяти (не учтены): 2"} {
		if !strings.Contains(mem, want) {
			t.Errorf("memory alert lacks %q:\n%s", want, mem)
		}
	}
}

func TestShortImage(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"nginx:alpine":          "nginx:alpine",
		"ghcr.io/acme/bot:v1.2": "bot:v1.2",
		"ghcr.io/acme/bot@sha256:0123456789abcdef": "bot@0123456789ab",
		"registry:5000/team/api:1.0@sha256:ff":     "api:1.0",
		"krill-12:345":                             "#345",
	}
	for in, want := range cases {
		if got := shortImage(in); got != want {
			t.Errorf("shortImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{512: "512 B", 1536: "1.5 KiB", 300 << 20: "300 MiB", 1932735283: "1.8 GiB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDiskDecide(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	hourAgo, dayAgo := now.Add(-time.Hour), now.Add(-25*time.Hour)
	cases := []struct {
		name string
		st   AlertState
		pct  float64
		want diskAction
	}{
		{"crosses the threshold", AlertState{}, 85, diskAlert},
		{"below, never alerted", AlertState{}, 70, diskNone},
		{"still high, alerted an hour ago", AlertState{Active: true, LastSent: &hourAgo}, 90, diskNone},
		{"still high a day later", AlertState{Active: true, LastSent: &dayAgo}, 90, diskAlert},
		{"just under, within hysteresis", AlertState{Active: true, LastSent: &hourAgo}, 83, diskNone},
		{"freed up", AlertState{Active: true, LastSent: &hourAgo}, 81.9, diskRecover},
		{"high again after a recovery", AlertState{Active: false, LastSent: &hourAgo}, 86, diskAlert},
	}
	for _, c := range cases {
		if got := diskDecide(c.st, c.pct, 85, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// flakySend fails while fail is set.
type flakySend struct {
	capturedSend
	fail bool
}

func (f *flakySend) fn(ctx context.Context, tok, chat, text string) error {
	f.mu.Lock()
	failing := f.fail
	f.mu.Unlock()
	if failing {
		return errors.New("telegram: 502")
	}
	return f.capturedSend.fn(ctx, tok, chat, text)
}

func diskTick(start time.Time, pct int64) metrics.Tick {
	return metrics.Tick{Start: start, Disks: true, Nodes: []metrics.NodeSample{{
		Node: "cp", OK: true, DisksOK: true,
		Disks: []metrics.DiskUsage{{Mount: "/", Total: 100, Used: pct, Avail: 100 - pct}},
	}}}
}

func TestMonitorDiskAlertRepeatAndRecovery(t *testing.T) {
	st := &fakeStore{infra: []Channel{{ID: 1, Enabled: true, ChatID: "ops", NotifyInfra: true, DiskThresholdPct: 85}}}
	send := &flakySend{}
	svc := New(st)
	svc.send = send.fn
	mon := NewResourceMonitor(svc, st)
	clock := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	mon.now = func() time.Time { return clock }
	step := func(pct int64) {
		mon.Observe(context.Background(), diskTick(clock, pct))
		mon.wg.Wait()
	}

	step(90)
	if send.count() != 1 || !strings.Contains(send.all()[0], "Disk almost full") {
		t.Fatalf("first crossing: %v", send.all())
	}
	clock = clock.Add(time.Hour)
	step(91)
	if send.count() != 1 {
		t.Fatalf("repeated within a day: %v", send.all())
	}
	clock = clock.Add(24 * time.Hour)
	step(92)
	if send.count() != 2 {
		t.Fatalf("not repeated after a day: %v", send.all())
	}
	clock = clock.Add(time.Hour)
	step(84) // under the threshold but within the hysteresis band
	if send.count() != 2 {
		t.Fatalf("recovery sent inside the hysteresis band: %v", send.all())
	}
	// A failed send leaves the state alone, so the next snapshot retries.
	send.mu.Lock()
	send.fail = true
	send.mu.Unlock()
	step(70)
	send.mu.Lock()
	send.fail = false
	send.mu.Unlock()
	step(70)
	if send.count() != 3 || !strings.Contains(send.all()[2], "back to normal") {
		t.Fatalf("recovery not sent after the failed attempt: %v", send.all())
	}
	step(70)
	if send.count() != 3 {
		t.Fatalf("recovery repeated: %v", send.all())
	}
}

// Ticks without a disk snapshot and without due checks must not touch the
// database at all (the sampler ticks every 30 seconds).
func TestMonitorIdleTickDoesNothing(t *testing.T) {
	st := &countingStore{fakeStore: &fakeStore{}}
	mon := NewResourceMonitor(New(st), st)
	mon.Observe(context.Background(), metrics.Tick{Start: time.Now()})
	mon.wg.Wait()
	if st.infraCalls != 0 {
		t.Fatalf("idle tick loaded channels %d times", st.infraCalls)
	}
}

type countingStore struct {
	*fakeStore
	infraCalls int
}

func (c *countingStore) InfraChannels(ctx context.Context) ([]Channel, error) {
	c.infraCalls++
	return c.fakeStore.InfraChannels(ctx)
}

func TestMonitorOvercommitAfterDeploy(t *testing.T) {
	st := &fakeStore{infra: []Channel{
		{ID: 1, OrgID: 7, Enabled: true, ChatID: "strict", NotifyInfra: true, MemThresholdPct: 100},
		{ID: 2, OrgID: 7, Enabled: true, ChatID: "lax", NotifyInfra: true, MemThresholdPct: 150},
		{ID: 3, OrgID: 9, Enabled: true, ChatID: "other-org", NotifyInfra: true, MemThresholdPct: 100},
	}}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	mon := NewResourceMonitor(svc, st)
	deployed := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	mon.QueueOvercommitCheck(5, Target{OrgID: 7, Project: "p", Env: "e", Name: "api"}, deployed)

	// A tick that started before the deploy finished cannot show its container.
	mon.Observe(context.Background(), metrics.Tick{Start: deployed.Add(-time.Second)})
	mon.wg.Wait()
	if cap.count() != 0 || len(mon.pending) != 1 {
		t.Fatalf("checked on a tick from before the deploy: %v %v", cap.all(), mon.pending)
	}

	node := metrics.NodeSample{
		Node: "worker-1", OK: true, CapacityOK: true, Capacity: docker.NodeInfo{MemTotal: 1000},
		Containers: []docker.ContainerStat{
			{Component: docker.ServiceName(5), MemLimitBytes: 600},
			{Component: "krill-9", MemLimitBytes: 500},
			{Component: "krill-traefik", MemLimitBytes: 1000}, // no limit: reports the host's memory
		},
	}
	other := metrics.NodeSample{Node: "cp", OK: true, CapacityOK: true, Capacity: docker.NodeInfo{MemTotal: 100},
		Containers: []docker.ContainerStat{{Component: "krill-1", MemLimitBytes: 90}, {Component: "krill-2", MemLimitBytes: 90}}}
	mon.Observe(context.Background(), metrics.Tick{Start: deployed.Add(30 * time.Second), Nodes: []metrics.NodeSample{other, node}})
	mon.wg.Wait()
	msgs := cap.all()
	sort.Strings(msgs)
	if len(msgs) != 2 || !strings.HasPrefix(msgs[0], "other-org|") || !strings.HasPrefix(msgs[1], "strict|") {
		t.Fatalf("sent %v, want alerts to the two 100%% channels only", msgs)
	}
	// Another organization's infrastructure channel learns about the node,
	// never the name of the app that was deployed.
	if strings.Contains(msgs[0], "api") || strings.Contains(msgs[0], "after the deploy") || !strings.Contains(msgs[0], "worker-1") {
		t.Errorf("other organization's alert:\n%s", msgs[0])
	}
	msgs = msgs[1:]
	for _, want := range []string{"worker-1", "110%", "p/e/api", "(not counted): 1"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("alert lacks %q:\n%s", want, msgs[0])
		}
	}
	if len(mon.pending) != 0 {
		t.Fatalf("check not cleared: %v", mon.pending)
	}
}

func TestMonitorDropsCheckWhenContainerNeverAppears(t *testing.T) {
	st := &fakeStore{infra: []Channel{{ID: 1, Enabled: true, NotifyInfra: true, MemThresholdPct: 100}}}
	mon := NewResourceMonitor(New(st), st)
	at := time.Now()
	mon.QueueOvercommitCheck(5, Target{}, at)
	for i := 0; i < overcommitTicks; i++ {
		mon.Observe(context.Background(), metrics.Tick{Start: at.Add(time.Duration(i+1) * time.Second)})
		mon.wg.Wait()
	}
	if len(mon.pending) != 0 {
		t.Fatalf("check kept after %d ticks: %v", overcommitTicks, mon.pending)
	}
}

// A newer deploy of the same app while a check is evaluated keeps the newer
// check rather than having it cleared by the older one's result.
func TestMonitorKeepsRequeuedCheck(t *testing.T) {
	st := &blockingInfra{fakeStore: &fakeStore{}, release: make(chan struct{}), entered: make(chan struct{})}
	mon := NewResourceMonitor(New(st), st)
	first := time.Now()
	mon.QueueOvercommitCheck(5, Target{Name: "old"}, first)
	mon.Observe(context.Background(), metrics.Tick{Start: first.Add(time.Second)})
	<-st.entered
	mon.QueueOvercommitCheck(5, Target{Name: "new"}, first.Add(2*time.Second))
	close(st.release)
	mon.wg.Wait()
	if p := mon.pending[5]; p.target.Name != "new" || p.ticks != 0 {
		t.Fatalf("pending = %+v, want the newer check untouched", p)
	}
}

type blockingInfra struct {
	*fakeStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockingInfra) InfraChannels(ctx context.Context) ([]Channel, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return []Channel{{ID: 1, NotifyInfra: true, MemThresholdPct: 100}}, nil
}

// A failed restore alerts like a failed backup but is not a backup run.
func TestRestoreFailureAlertsWithoutJournal(t *testing.T) {
	st := &fakeStore{
		channels:      map[int64][]Channel{7: {{ID: 1, Enabled: true, ChatID: "c", NotifyBackup: true}}},
		volumeBackups: map[int64]Target{4: {OrgID: 7, Name: "web / data"}},
		bucketBackups: map[int64]Target{5: {OrgID: 7, Name: "assets"}},
	}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	svc.VolumeRestoreFailed(context.Background(), 4, "volume restore: scale-up failed")
	svc.BucketRestoreFailed(context.Background(), 5, "restore: denied")
	waitUntil(t, func() bool { return cap.count() == 2 })
	if runs := st.recordedRuns(); len(runs) != 0 {
		t.Fatalf("restores journalled as backup runs: %+v", runs)
	}
	for _, m := range cap.all() {
		if !strings.Contains(m, "Backup failed") {
			t.Errorf("restore alert changed: %s", m)
		}
	}
}
