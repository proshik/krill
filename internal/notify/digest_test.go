package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/metrics"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDueAt(t *testing.T) {
	c := Channel{DigestMinute: 11 * 60, DigestTZ: "Europe/Moscow"} // 11:00 MSK = 08:00 UTC
	at := func(h, m int) time.Time { return time.Date(2026, 11, 18, h, m, 0, 0, time.UTC) }
	sched := at(8, 0)
	if _, ok := dueAt(c, at(7, 59)); ok {
		t.Error("due a minute early")
	}
	if got, ok := dueAt(c, at(8, 0)); !ok || !got.Equal(time.Date(2026, 11, 18, 0, 0, 0, 0, mustLoc(t, "Europe/Moscow"))) {
		t.Errorf("not due at 11:00 MSK, or not for that local day: %v %v", got, ok)
	}
	if _, ok := dueAt(c, at(10, 59)); !ok {
		t.Error("a missed digest is not sent within the late window")
	}
	if _, ok := dueAt(c, at(11, 0)); ok {
		t.Error("due 3 hours late: it would arrive in the evening/night")
	}
	sent := at(8, 1)
	c.DigestLastAt = &sent
	if _, ok := dueAt(c, at(8, 5)); ok {
		t.Error("sent twice on one day")
	}
	yesterday := sched.Add(-24 * time.Hour)
	c.DigestLastAt = &yesterday
	if _, ok := dueAt(c, at(8, 5)); !ok {
		t.Error("yesterday's digest blocks today's")
	}
	// Today's digest went out at 11:00; then the time is moved to 14:00.
	c.DigestLastAt = &sent
	c.DigestMinute = 14 * 60
	if _, ok := dueAt(c, at(11, 0)); ok {
		t.Error("moving the time after today's digest sends a second one")
	}
}

// The scheduled time is wall-clock time in the channel's zone, across a DST
// change too.
func TestDueAtFollowsDST(t *testing.T) {
	c := Channel{DigestMinute: 11 * 60, DigestTZ: "Europe/Amsterdam"}
	// 2026-03-29: CET → CEST at 02:00, so 11:00 local is 09:00 UTC.
	if _, ok := dueAt(c, time.Date(2026, 3, 29, 9, 0, 0, 0, time.UTC)); !ok {
		t.Fatal("not due at 11:00 local (09:00 UTC)")
	}
	if _, ok := dueAt(c, time.Date(2026, 3, 29, 8, 59, 0, 0, time.UTC)); ok {
		t.Fatal("due at 10:59 local")
	}
}

func TestValidTZ(t *testing.T) {
	for _, tz := range []string{"Europe/Moscow", "UTC", "Asia/Tbilisi"} {
		if !ValidTZ(tz) {
			t.Errorf("%s rejected", tz)
		}
	}
	for _, tz := range []string{"", "Local", "Mars/Olympus", "../etc/passwd"} {
		if ValidTZ(tz) {
			t.Errorf("%q accepted", tz)
		}
	}
}

type fakeMetrics struct {
	samples []metrics.Sample
	caps    []metrics.NodeCapacity
	disks   []metrics.NodeDisk
}

func (f fakeMetrics) Since(context.Context, time.Time) ([]metrics.Sample, error) {
	return f.samples, nil
}
func (f fakeMetrics) ListCapacity(context.Context) ([]metrics.NodeCapacity, error) {
	return append([]metrics.NodeCapacity(nil), f.caps...), nil
}
func (f fakeMetrics) ListDisks(context.Context) ([]metrics.NodeDisk, error) { return f.disks, nil }

type fakeTasks map[string]int

func (f fakeTasks) TaskFailuresSince(context.Context, time.Time) (map[string]int, error) {
	return f, nil
}

// digestFixture is one organization with two apps on a two-node cluster.
func digestFixture(now time.Time) (*fakeStore, fakeMetrics) {
	const gib = int64(1) << 30
	st := &fakeStore{
		orgApps: []WatchedApp{
			{AppID: 5, OrgID: 7, Project: "p", Env: "prod", Name: "api"},
			{AppID: 6, OrgID: 7, Project: "p", Env: "prod", Name: "bot"},
			{AppID: 8, OrgID: 7, Project: "p", Env: "dev", Name: "old"},
		},
		orgDeploys: []DeploymentInfo{
			{AppID: 5, Project: "p", Env: "prod", App: "api", Status: "done", Trigger: "manual", ImageTag: "ghcr.io/a/api:v2", FinishedAt: now.Add(-2 * time.Hour)},
			{AppID: 6, Project: "p", Env: "prod", App: "bot", Status: "error", Trigger: "webhook", Error: "service did not converge", FinishedAt: now.Add(-26 * time.Hour / 2)},
			{AppID: 6, Project: "p", Env: "prod", App: "bot", Status: "done", Trigger: "system", FinishedAt: now.Add(-time.Hour)},
		},
		runs: []BackupRun{
			{OrgID: 7, Kind: BackupKindDatabase, Project: "p", Env: "prod", Target: "krill-postgres-x", Status: "ok", SizeBytes: 12 << 20, FinishedAt: now.Add(-8 * time.Hour)},
			{OrgID: 7, Kind: BackupKindBucket, Project: "p", Env: "prod", Target: "assets", Status: "error", Error: "list bucket: denied", FinishedAt: now.Add(-8 * time.Hour)},
			{OrgID: 9, Kind: BackupKindDatabase, Target: "other-org", Status: "ok", FinishedAt: now.Add(-time.Hour)},
		},
	}
	var ss []metrics.Sample
	tick := func(at time.Time, api, bot int64, cpu float64) {
		ss = append(ss,
			metrics.Sample{Node: "control-plane", Component: "krill-5", TS: at, MemBytes: api, MemLimitBytes: 512 << 20, CPUPct: cpu},
			metrics.Sample{Node: "control-plane", Component: "krill-traefik", TS: at.Add(time.Millisecond), MemBytes: 50 << 20, MemLimitBytes: 2 * gib, CPUPct: 0},
			metrics.Sample{Node: "worker-1", Component: "krill-6", TS: at, MemBytes: bot, MemLimitBytes: 256 << 20, CPUPct: cpu / 2},
		)
	}
	tick(now.Add(-3*time.Hour), 300<<20, 100<<20, 160) // the peak
	tick(now.Add(-30*time.Second), 200<<20, 240<<20, 40)
	m := fakeMetrics{
		samples: ss,
		caps: []metrics.NodeCapacity{
			{Node: "worker-1", NCPU: 2, MemTotal: gib, SampledAt: now.Add(-20 * time.Second)},
			{Node: "control-plane", NCPU: 2, MemTotal: 2 * gib, SampledAt: now.Add(-20 * time.Second)},
		},
		disks: []metrics.NodeDisk{
			{Node: "control-plane", DiskUsage: metrics.DiskUsage{Mount: "/", Total: 50 * gib, Used: 44 * gib, Avail: 6 * gib}},
			{Node: "worker-1", DiskUsage: metrics.DiskUsage{Mount: "/", Total: 20 * gib, Used: 5 * gib, Avail: 15 * gib}},
		},
	}
	return st, m
}

func TestDigestBuildInfraChannel(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	st, m := digestFixture(now)
	svc := New(st)
	svc.SetInstanceName(func(context.Context) string { return "krill.example.com" })
	d := NewDigest(svc, st, m, fakeTasks{"krill-6": 3})
	parts, err := d.Build(context.Background(), Channel{
		OrgID: 7, Lang: "ru", DigestTZ: "Europe/Moscow", NotifyInfra: true, DiskThresholdPct: 85, MemThresholdPct: 100,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("want one part, got %d", len(parts))
	}
	got := parts[0]
	t.Logf("digest:\n%s", got)
	for _, want := range []string{
		"Сводка Krill за сутки · krill.example.com",
		"11:00 (Europe/Moscow)",
		"Требует внимания",
		"деплой упал: p/prod/bot",
		"бэкап упал: бакет p/prod/assets",
		"p/prod/bot: перезапусков 3",
		"p/prod/bot: пик памяти 94% от лимита",
		"нода control-plane, диск /: занято 88%",
		"Деплои за 24 ч</b>: успешно 1, с ошибкой 1",
		"✅ p/prod/api · api:v2 · 09:00",
		"❌ p/prod/bot · 17.11 22:00 · service did not converge",
		"Бэкапы за 24 ч</b>: успешно 1, с ошибкой 1",
		"✅ база p/prod/krill-postgres-x · 12.0 MiB · 03:00",
		"<b>control-plane</b>",
		"диск /: 88% · 44.0 GiB из 50.0 GiB",
		"лимиты 512 MiB (25%) · без лимита: 1",
		"CPU ср. 50% · пик 80% · ядер 2",
		"p/prod/api: 200 MiB · пик 300 MiB из 512 MiB (59%) · перезапусков 0",
		"Не запущены: p/dev/old",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q", want)
		}
	}
	for _, leak := range []string{"other-org", "system"} {
		if strings.Contains(got, leak) {
			t.Errorf("digest shows %q", leak)
		}
	}
	if strings.Index(got, "Требует внимания") > strings.Index(got, "Деплои") {
		t.Error("the attention block must come first")
	}
}

// Without the infrastructure flag the digest says nothing about nodes, which
// are shared by every organization.
func TestDigestWithoutInfraHasNoNodes(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	st, m := digestFixture(now)
	d := NewDigest(New(st), st, m, nil)
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, Lang: "en", DigestTZ: "UTC", DiskThresholdPct: 85, MemThresholdPct: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(parts, "\n")
	for _, leak := range []string{"Nodes", "control-plane", "worker-1", "CPU", "disk"} {
		if strings.Contains(got, leak) {
			t.Errorf("non-infrastructure digest shows %q:\n%s", leak, got)
		}
	}
	if strings.Contains(got, "restarts") {
		t.Errorf("restarts shown without a task source:\n%s", got)
	}
}

func TestDigestAllGood(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	st := &fakeStore{}
	d := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, Lang: "en", DigestTZ: "UTC", DiskThresholdPct: 85, MemThresholdPct: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := parts[0]; !strings.Contains(got, "✅ All good") || !strings.Contains(got, "Deploys, 24h</b>: 0 ok, 0 failed") {
		t.Fatalf("empty digest:\n%s", got)
	}
}

func TestDigestEscapesNames(t *testing.T) {
	now := time.Now()
	st := &fakeStore{orgDeploys: []DeploymentInfo{{Project: "<b>x</b>", Env: "e", App: "a&b", Status: "done", FinishedAt: now}}}
	d := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, _ := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", DiskThresholdPct: 85, MemThresholdPct: 100}, now)
	if got := parts[0]; strings.Contains(got, "<b>x</b>") || !strings.Contains(got, "&lt;b&gt;x&lt;/b&gt;/e/a&amp;b") {
		t.Fatalf("names not escaped:\n%s", got)
	}
}

// A node with no containers has no samples but is reporting (its capacity
// row is fresh); one whose capacity row is old is not.
func TestNodeRowsStaleness(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	rows := nodeRows([]metrics.NodeCapacity{
		{Node: "empty", NCPU: 1, MemTotal: 1 << 30, SampledAt: now.Add(-30 * time.Second)},
		{Node: "gone", NCPU: 1, MemTotal: 1 << 30, SampledAt: now.Add(-time.Hour)},
	}, nil, []metrics.Sample{{Node: "gone", Component: "krill-1", TS: now.Add(-time.Hour), MemBytes: 100, MemLimitBytes: 200}}, now)
	if rows[0].Name != "empty" || rows[0].Stale {
		t.Errorf("a node without containers is reported stale: %+v", rows[0])
	}
	if rows[1].Name != "gone" || !rows[1].Stale || rows[1].MemUsed != 0 {
		t.Errorf("a node that stopped reporting: %+v (its last memory must not read as current)", rows[1])
	}
}

func TestSplitParts(t *testing.T) {
	lines := []string{strings.Repeat("a", 6), strings.Repeat("b", 6), strings.Repeat("c", 6)}
	got := splitParts(lines, 13)
	if len(got) != 2 || got[0] != "aaaaaa\nbbbbbb" || got[1] != "cccccc" {
		t.Fatalf("splitParts = %q", got)
	}
	if got := splitParts([]string{strings.Repeat("x", 20)}, 10); len(got) != 1 || len(got[0]) != 10 {
		t.Fatalf("an over-long line = %q", got)
	}
	if got := splitParts([]string{"aaaaaa", "", "bbbbbbbbbbbb"}, 13); len(got) != 2 || got[1] != "bbbbbbbbbbbb" {
		t.Fatalf("a part starts with a blank line: %q", got)
	}
}

func TestDigestTickClaimsOnceAndRetriesFailedSend(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 30, 0, time.UTC)
	ch := Channel{ID: 1, OrgID: 7, Enabled: true, DigestEnabled: true, ChatID: "c", DigestMinute: 660, DigestTZ: "Europe/Moscow", DiskThresholdPct: 85, MemThresholdPct: 100}
	st := &fakeStore{digest: []Channel{ch}}
	send := &flakySend{fail: true}
	svc := New(st)
	svc.send = send.fn
	d := NewDigest(svc, st, fakeMetrics{}, nil)
	d.now = func() time.Time { return now }

	d.tick(context.Background())
	if send.count() != 0 || st.released != 1 {
		t.Fatalf("failed send: sent %d, released %d; want the claim undone", send.count(), st.released)
	}
	send.mu.Lock()
	send.fail = false
	send.mu.Unlock()
	now = now.Add(time.Minute)
	d.tick(context.Background())
	if send.count() != 1 {
		t.Fatalf("not retried: %d", send.count())
	}
	// The scheduler re-reads the channels each tick; simulate the stored claim.
	last := st.claims[1]
	st.digest[0].DigestLastAt = &last
	now = now.Add(time.Minute)
	d.tick(context.Background())
	if send.count() != 1 {
		t.Fatalf("sent twice: %d", send.count())
	}
}

// Two passes racing on one occurrence (two ticks, or a restart) must send
// once: the claim, not the channel snapshot, decides.
func TestDigestClaimGuardsAStaleSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 30, 0, time.UTC)
	ch := Channel{ID: 1, OrgID: 7, Enabled: true, DigestEnabled: true, DigestMinute: 660, DigestTZ: "Europe/Moscow", DiskThresholdPct: 85, MemThresholdPct: 100}
	st := &fakeStore{digest: []Channel{ch}} // the snapshot never shows the claim
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	d := NewDigest(svc, st, fakeMetrics{}, nil)
	d.now = func() time.Time { return now }
	d.tick(context.Background())
	now = now.Add(time.Minute)
	d.tick(context.Background())
	if cap.count() != 1 {
		t.Fatalf("sent %d times", cap.count())
	}
}

type errMetrics struct{ fakeMetrics }

func (errMetrics) Since(context.Context, time.Time) ([]metrics.Sample, error) {
	return nil, errors.New("db down")
}

func TestDigestBuildFailureReleasesClaim(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 30, 0, time.UTC)
	ch := Channel{ID: 1, OrgID: 7, Enabled: true, DigestEnabled: true, DigestMinute: 660, DigestTZ: "Europe/Moscow"}
	st := &fakeStore{digest: []Channel{ch}}
	cap := &capturedSend{}
	svc := New(st)
	svc.send = cap.fn
	d := NewDigest(svc, st, errMetrics{}, nil)
	d.now = func() time.Time { return now }
	d.tick(context.Background())
	if cap.count() != 0 || st.released != 1 {
		t.Fatalf("sent %d, released %d", cap.count(), st.released)
	}
}

func TestSplitTicks(t *testing.T) {
	base := time.Unix(1000, 0)
	ss := []metrics.Sample{
		{TS: base.Add(30 * time.Second)}, {TS: base}, {TS: base.Add(2 * time.Millisecond)}, {TS: base.Add(30*time.Second + time.Millisecond)},
	}
	got := splitTicks(ss)
	if len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 2 {
		t.Fatalf("splitTicks = %v", got)
	}
}

// A digest that went out late (13:30 instead of 11:00 yesterday) must not
// leave the deploys of 11:00–13:30 out of today's digest.
func TestDigestEventsStartAtThePreviousDigest(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	rec := &sinceStore{fakeStore: &fakeStore{}}
	d := NewDigest(New(rec), rec, fakeMetrics{}, nil)
	late := now.Add(-24*time.Hour + 150*time.Minute) // yesterday 13:30 MSK
	early := now.Add(-21 * time.Hour)
	for _, c := range []struct {
		last *time.Time
		want time.Time
	}{
		{nil, now.Add(-24 * time.Hour)},
		{&late, now.Add(-24 * time.Hour)}, // within the window: nothing missed
		{&early, now.Add(-24 * time.Hour)},
	} {
		if _, err := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", DigestLastAt: c.last}, now); err != nil {
			t.Fatal(err)
		}
		if !rec.since.Equal(c.want) {
			t.Errorf("last %v: deploys since %v, want %v", c.last, rec.since, c.want)
		}
	}
	lateYesterday := now.Add(-26 * time.Hour)
	if _, err := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", DigestLastAt: &lateYesterday}, now); err != nil {
		t.Fatal(err)
	}
	if !rec.since.Equal(lateYesterday) || !rec.runsSince.Equal(lateYesterday) {
		t.Errorf("after a gap: deploys since %v, runs since %v, want %v", rec.since, rec.runsSince, lateYesterday)
	}
	weekAgo := now.Add(-7 * 24 * time.Hour)
	d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", DigestLastAt: &weekAgo}, now)
	if !rec.since.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("after a week's pause: deploys since %v, want the last 24h", rec.since)
	}
}

type sinceStore struct {
	*fakeStore
	since, runsSince time.Time
}

func (s *sinceStore) OrgDeploymentsSince(ctx context.Context, org int64, since time.Time) ([]DeploymentInfo, error) {
	s.since = since
	return nil, nil
}
func (s *sinceStore) BackupRunsSince(ctx context.Context, org int64, since time.Time) ([]BackupRun, error) {
	s.runsSince = since
	return nil, nil
}

func TestDigestCapsStoppedApps(t *testing.T) {
	st := &fakeStore{}
	for i := 0; i < 150; i++ {
		st.orgApps = append(st.orgApps, WatchedApp{AppID: int64(i + 1), Project: "p&q", Env: "e", Name: "app" + strings.Repeat("x", 30)})
	}
	d := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", Lang: "en"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(parts, "\n")
	if !strings.Contains(got, "…and 130 more") || strings.Count(got, "p&amp;q/e/") != 20 {
		t.Fatalf("stopped list not capped:\n%s", got)
	}
}

func TestCutLineKeepsEntities(t *testing.T) {
	if got := cutLine([]rune("abc&amp;def"), 6); got != "abc" {
		t.Errorf("cut inside an entity: %q", got)
	}
	if got := cutLine([]rune("abc&amp;def"), 9); got != "abc&amp;d" {
		t.Errorf("cut after an entity: %q", got)
	}
}
