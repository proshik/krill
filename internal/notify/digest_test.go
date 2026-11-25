package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
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

type fakeSwarm struct {
	fails  map[string]int
	states map[string]docker.ServiceState
}

func (f fakeSwarm) TaskFailuresSince(context.Context, time.Time) (map[string]int, error) {
	return f.fails, nil
}

func (f fakeSwarm) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	out := map[string]docker.ServiceState{}
	for _, n := range names {
		if st, ok := f.states[n]; ok {
			out[n] = st
		}
	}
	return out, nil
}

// fixtureSwarm: bot crashed 3 times; old was stopped on purpose; worker
// should run and runs nothing; never was never deployed.
func fixtureSwarm() fakeSwarm {
	return fakeSwarm{
		fails: map[string]int{"krill-6": 3},
		states: map[string]docker.ServiceState{
			"krill-5":  {Found: true, Desired: 1, Running: 1},
			"krill-6":  {Found: true, Desired: 1, Running: 1},
			"krill-8":  {Found: true, Desired: 0},
			"krill-10": {Found: true, Desired: 1},
		},
	}
}

// digestFixture is one organization with two apps on a two-node cluster.
func digestFixture(now time.Time) (*fakeStore, fakeMetrics) {
	const gib = int64(1) << 30
	st := &fakeStore{
		orgApps: []WatchedApp{
			{AppID: 5, OrgID: 7, Project: "p", Env: "prod", Name: "api"},
			{AppID: 6, OrgID: 7, Project: "p", Env: "prod", Name: "bot"},
			{AppID: 8, OrgID: 7, Project: "p", Env: "dev", Name: "old"},
			{AppID: 10, OrgID: 7, Project: "p", Env: "prod", Name: "worker"},
			{AppID: 11, OrgID: 7, Project: "p", Env: "dev", Name: "never"},
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
	d := NewDigest(svc, st, m, fixtureSwarm())
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
		"<b>📋 Сводка Krill за сутки</b>\nkrill.example.com · 18.11, 11:00 (Europe/Moscow)",
		"🔴 <b>Требует внимания</b>",
		"🔴 деплой упал: p/prod/bot",
		"🔴 бэкап упал: бакет p/prod/assets",
		"🔴 p/prod/bot: 3 падения",
		"🟡 p/prod/bot: пик памяти 94% от лимита",
		"🔴 p/prod/worker: не запущено",
		"🔴 нода control-plane, диск /: занято 88%",
		"🚀 <b>Деплои</b>: 2, успешно 1, с ошибкой 1\n<b>p · prod</b>\n",
		"🟢 api — 1 деплой, сейчас api:v2 (09:00)",
		"🔴 bot — 17.11 22:00 · service did not converge",
		"💾 <b>Бэкапы</b>: 2, успешно 1, с ошибкой 1\n<b>p · prod</b>\n",
		"🟢 база krill-postgres-x · 12.0 MiB · 03:00",
		"🔴 бакет assets · 03:00 · list bucket: denied",
		"🔴 <b>control-plane</b> — диск 88% · память 12%, лимиты 25% · CPU 50% (пик 80%)\n↳ диск / занят 88%\n",
		"🟢 <b>worker-1</b> — диск 25% · память 23%, лимиты 25% · CPU 25% (пик 40%)\n<blockquote expandable><b>control-plane</b>\n🔴 диск /: занято 88% (44.0 из 50.0 GiB), свободно 6.0 GiB",
		"\nпамять: контейнеры занимают 250 MiB из 2.0 GiB · сумма лимитов 512 MiB — 25% памяти ноды · 1 контейнер без лимита",
		"\nCPU: в среднем 50%, пик 80% — от мощности всех 2 ядер",
		"всех 2 ядер</blockquote>\n\n📦 <b>Приложения</b>\n",
		"🟢 api — 200 MiB, пик 300 из 512 MiB (59%)\n",
		"🔴 bot — 240 MiB, пик 240 из 256 MiB (94%) · 3 падения",
		"📦 <b>Приложения</b>\n<b>p · dev</b>\n⚪ Остановлены или не развёрнуты: old, never\n<b>p · prod</b>\n",
		"· 3 падения\n🔴 Не запущены: worker",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q", want)
		}
	}
	for _, leak := range []string{"other-org", "system", "0 падений"} {
		if strings.Contains(got, leak) {
			t.Errorf("digest shows %q", leak)
		}
	}
	if strings.Index(got, "Требует внимания") > strings.Index(got, "Деплои") {
		t.Error("the attention block must come first")
	}
	if strings.Index(got, "🔴 нода control-plane") > strings.Index(got, "🟡 p/prod/bot") {
		t.Error("red items must come before yellow ones")
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
	if strings.Contains(got, "crashes") {
		t.Errorf("crashes shown without a task source:\n%s", got)
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
	if got := parts[0]; !strings.Contains(got, "🟢 All good") || !strings.Contains(got, "🚀 <b>Deploys</b>: none") || !strings.Contains(got, "💾 <b>Backups</b>: none") {
		t.Fatalf("empty digest:\n%s", got)
	}
}

func TestDigestEscapesNames(t *testing.T) {
	now := time.Now()
	st := &fakeStore{orgDeploys: []DeploymentInfo{{Project: "<b>x</b>", Env: "e", App: "a&b", Status: "done", FinishedAt: now}}}
	d := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, _ := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", DiskThresholdPct: 85, MemThresholdPct: 100}, now)
	if got := parts[0]; strings.Contains(got, "<b>x</b>") || !strings.Contains(got, "<b>&lt;b&gt;x&lt;/b&gt; · e</b>\n🟢 a&amp;b — ") {
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
	if !strings.Contains(got, "<b>p&amp;q · e</b>\n⚪ No fresh data: ") || !strings.Contains(got, "…and 130 more") || strings.Count(got, "appxxx") != 20 {
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

func TestTrafficLightBands(t *testing.T) {
	d := digestData{DiskThreshold: 85, MemThreshold: 100}
	const gib = int64(1) << 30
	disk := func(pct int64) metrics.DiskUsage { return metrics.DiskUsage{Used: pct, Avail: 100 - pct} }
	for _, c := range []struct {
		name string
		got  level
		want level
	}{
		{"disk 69%", d.diskLevel(disk(69)), lvlOK},
		{"disk 70% (threshold-15)", d.diskLevel(disk(70)), lvlWarn},
		{"disk 85% (threshold)", d.diskLevel(disk(85)), lvlCrit},
		{"limits 79%", d.limitsLevel(nodeRow{MemTotal: 100, Limits: 79}), lvlOK},
		{"limits 80%", d.limitsLevel(nodeRow{MemTotal: 100, Limits: 80}), lvlWarn},
		{"limits 100% (threshold)", d.limitsLevel(nodeRow{MemTotal: 100, Limits: 100}), lvlCrit},
		{"limits, threshold 70 under the yellow mark", digestData{MemThreshold: 70}.limitsLevel(nodeRow{MemTotal: 100, Limits: 70}), lvlCrit},
		{"containers use 80% of the node", memUsedLevel(nodeRow{MemTotal: 100, MemUsed: 80}), lvlWarn},
		{"containers use 90% of the node", memUsedLevel(nodeRow{MemTotal: 100, MemUsed: 90}), lvlCrit},
		{"app peak 79%", appRow{Peak: 79, Limit: 100}.level(), lvlOK},
		{"app peak 80%", appRow{Peak: 80, Limit: 100}.level(), lvlWarn},
		{"app peak 79.6%, printed as 80%", appRow{Peak: 796, Limit: 1000}.level(), lvlWarn},
		{"app peak 95%", appRow{Peak: 95, Limit: 100}.level(), lvlCrit},
		{"app without a limit", appRow{Peak: gib}.level(), lvlOK},
		{"app that crashed", appRow{Peak: 1, Limit: 100, Restarts: 1}.level(), lvlCrit},
		{"node CPU avg 69%, peak 100%", cpuLevel(nodeRow{CPUKnown: true, CPUAvg: 69, CPUPeak: 100}), lvlOK},
		{"node CPU avg 70%", cpuLevel(nodeRow{CPUKnown: true, CPUAvg: 70}), lvlWarn},
		{"node CPU avg 90%", cpuLevel(nodeRow{CPUKnown: true, CPUAvg: 90}), lvlCrit},
		{"stale node", d.nodeLevel(nodeRow{Stale: true, Disks: []metrics.DiskUsage{disk(1)}}), lvlCrit},
		{"node without disk data", d.nodeLevel(nodeRow{MemTotal: gib}), lvlWarn},
	} {
		if c.got != c.want {
			t.Errorf("%s: %s, want %s", c.name, c.got.mark(), c.want.mark())
		}
	}
}

// The first production digest (ams, 2026-11-19), reduced to one app and one
// node: every figure is green, and the empty backup journal says "none"
// rather than "0 ok, 0 failed".
func TestDigestAllGreen(t *testing.T) {
	now := time.Date(2026, 11, 19, 9, 50, 0, 0, time.UTC)
	const mib = int64(1) << 20
	st := &fakeStore{
		orgApps: []WatchedApp{{AppID: 3, OrgID: 1, Project: "side", Env: "prod", Name: "pochitushki"}},
		orgDeploys: []DeploymentInfo{
			{AppID: 3, Project: "side", Env: "prod", App: "pochitushki", Status: "done", Trigger: "webhook", ImageTag: "ghcr.io/x/pochitushki@sha256:cf691d6cd4d6aaaa", FinishedAt: now.Add(-20 * time.Hour)},
		},
	}
	m := fakeMetrics{
		samples: []metrics.Sample{
			{Node: "krill-worker-ams", Component: "krill-3", TS: now.Add(-30 * time.Second), MemBytes: 291 * mib, MemLimitBytes: 768 * mib, CPUPct: 2},
		},
		caps:  []metrics.NodeCapacity{{Node: "krill-worker-ams", NCPU: 2, MemTotal: 3891 * mib, SampledAt: now.Add(-20 * time.Second)}},
		disks: []metrics.NodeDisk{{Node: "krill-worker-ams", DiskUsage: metrics.DiskUsage{Mount: "/", Total: 80 << 30, Used: 10 << 30, Avail: 65 << 30}}},
	}
	sw := fakeSwarm{fails: map[string]int{}, states: map[string]docker.ServiceState{"krill-3": {Found: true, Desired: 1, Running: 1}}}
	d := NewDigest(New(st), st, m, sw)
	parts, err := d.Build(context.Background(), Channel{OrgID: 1, Lang: "ru", DigestTZ: "Europe/Moscow", NotifyInfra: true, DiskThresholdPct: 85, MemThresholdPct: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := parts[0]
	t.Logf("digest:\n%s", got)
	for _, want := range []string{
		"🟢 Всё в норме",
		"💾 <b>Бэкапы</b>: не было",
		"🟢 <b>krill-worker-ams</b> — диск 14% · память 7%, лимиты 20% · CPU 1% (пик 1%)\n<blockquote expandable><b>krill-worker-ams</b>\nдиск /: занято 14% (10.0 из 80.0 GiB), свободно 65.0 GiB",
		"🟢 pochitushki — 1 деплой, сейчас pochitushki@cf691d6cd4d6 (",
		"<b>side · prod</b>\n🟢 pochitushki — 291 MiB, пик 291 из 768 MiB (38%)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q", want)
		}
	}
	if strings.Contains(got, "🟡") || strings.Contains(got, "🔴") {
		t.Errorf("a green digest shows yellow or red")
	}
}

// An app whose node lost its metrics but that Swarm still runs is "no fresh
// data", not "not running"; without Swarm nothing is called down.
func TestAppRowsIdleBySwarmState(t *testing.T) {
	now := time.Now()
	apps := []WatchedApp{{AppID: 1, Name: "a"}, {AppID: 2, Name: "b"}, {AppID: 3, Name: "c"}}
	_, idle := appRows(apps, nil, nil, nil, map[string]docker.ServiceState{
		"krill-1": {Found: true, Desired: 1, Running: 1},
		"krill-2": {Found: true, Desired: 1},
	}, now)
	if len(idle.NoData) != 1 || len(idle.Down) != 1 || len(idle.Stopped) != 1 {
		t.Fatalf("idle = %+v", idle)
	}
	_, idle = appRows(apps, nil, nil, nil, nil, now)
	if len(idle.NoData) != 3 || len(idle.Down) != 0 {
		t.Fatalf("without Swarm: %+v", idle)
	}
}

// A crash-looping app whose last sample is old keeps its limit: the OOM
// that kills it is visible as its peak against the limit.
func TestAppRowsCrashLoopKeepsLimit(t *testing.T) {
	now := time.Now()
	rows, _ := appRows([]WatchedApp{{AppID: 1, Name: "a"}}, []metrics.Sample{
		{Node: "n", Component: "krill-1", TS: now.Add(-20 * time.Minute), MemBytes: 256 << 20, MemLimitBytes: 256 << 20},
	}, map[string]int64{"n": 1 << 30}, map[string]int{"krill-1": 3}, nil, now)
	if len(rows) != 1 || rows[0].Limit != 256<<20 || rows[0].Now != 0 || rows[0].memLevel() != lvlCrit {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestBytesPairWritesASharedUnitOnce(t *testing.T) {
	ru, en := msgCtx("ru"), msgCtx("en")
	const mib, gib = int64(1) << 20, int64(1) << 30
	for _, c := range []struct{ got, want string }{
		{bytesPair(ru, "tg.pair", 12*gib+900*mib, 148*gib), "12.9 из 148 GiB"},
		{bytesPair(en, "tg.pair", 12*gib+900*mib, 148*gib), "12.9 of 148 GiB"},
		{bytesPair(ru, "tg.pair_limit", 584*mib, 1536*mib), "584 MiB из лимита 1.5 GiB"},
		{bytesPair(en, "tg.pair_limit", 240*mib, 256*mib), "240 of the 256 MiB limit"},
		{bytesPair(en, "tg.pair", 512, 2*gib), "512 B of 2.0 GiB"},
	} {
		if c.got != c.want {
			t.Errorf("%q, want %q", c.got, c.want)
		}
	}
}

// A yellow or red node's heading names what made it so; a green one is
// just the name.
func TestNodeHeadingNamesTheReason(t *testing.T) {
	const gib = int64(1) << 30
	d := digestData{DiskThreshold: 85, MemThreshold: 100, Lang: "ru"}
	ctx := msgCtx("ru")
	disk := func(mount string, pct int64) metrics.DiskUsage {
		return metrics.DiskUsage{Mount: mount, Used: pct, Avail: 100 - pct}
	}
	penpot := nodeRow{Name: "krill-penpot-msk", MemTotal: 73 * gib / 10, MemUsed: 26 * gib / 10, Limits: 78 * gib / 10,
		NCPU: 2, CPUKnown: true, CPUAvg: 1, CPUPeak: 3, Disks: []metrics.DiskUsage{disk("/", 10)}}
	if got := d.nodeWhy(ctx, penpot); got != "сумма лимитов 107% памяти ноды" {
		t.Errorf("one reason: %q", got)
	}
	db := penpot
	db.Disks = []metrics.DiskUsage{disk("/", 91), disk("/var/lib/docker", 70)}
	db.Limits = 104 * gib * 73 / 1000
	if got := d.nodeWhy(ctx, db); got != "диск / занят 91%, сумма лимитов 104% памяти ноды" {
		t.Errorf("two reds (the yellow docker disk left out): %q", got)
	}
	db.MemUsed, db.CPUAvg = db.MemTotal*95/100, 95
	if got := d.nodeWhy(ctx, db); got != "диск / занят 91%, сумма лимитов 104% памяти ноды, и ещё 2" {
		t.Errorf("four reds: %q", got)
	}
	green := penpot
	green.Limits = gib
	if got := d.nodeWhy(ctx, green); got != "" {
		t.Errorf("a green node has a reason: %q", got)
	}
	if got := d.nodeWhy(ctx, nodeRow{Stale: true}); got != "" {
		t.Errorf("a stale node gets a reason besides 'not reporting': %q", got)
	}

	d.Infra, d.Nodes, d.Loc, d.Now = true, []nodeRow{penpot, green}, time.UTC, time.Now()
	got := strings.Join(renderDigest(d), "\n")
	for _, want := range []string{
		"🔴 <b>krill-penpot-msk</b> — диск 10% · память 36%, лимиты 107% · CPU 1% (пик 3%)\n↳ сумма лимитов 107% памяти ноды\n",
		"🟢 <b>krill-penpot-msk</b> — диск 10%",
		"<blockquote expandable><b>krill-penpot-msk</b>\nдиск /: занято 10%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q:\n%s", want, got)
		}
	}
}

// A yellow node lists its yellow figures; a node whose disk snapshot failed
// is yellow for that alone; the English phrases render too.
func TestNodeHeadingYellowAndEnglish(t *testing.T) {
	const gib = int64(1) << 30
	d := digestData{DiskThreshold: 85, MemThreshold: 100}
	disk := metrics.DiskUsage{Mount: "/", Total: 100 * gib, Used: 75 * gib, Avail: 25 * gib} // 75%: yellow
	n := nodeRow{Name: "w", MemTotal: 10 * gib, MemUsed: 2 * gib, Limits: 85 * gib / 10, NCPU: 2, CPUKnown: true, CPUAvg: 1,
		Disks: []metrics.DiskUsage{disk}}
	if got := d.nodeWhy(msgCtx("ru"), n); got != "диск / занят 75%, сумма лимитов 85% памяти ноды" {
		t.Errorf("yellow node: %q", got)
	}
	if got := d.nodeWhy(msgCtx("en"), n); got != "disk / 75% used, memory limits at 85% of its memory" {
		t.Errorf("yellow node, en: %q", got)
	}
	n.Disks, n.Limits = nil, gib
	if got := d.nodeWhy(msgCtx("ru"), n); got != "нет данных о диске" || d.nodeLevel(n) != lvlWarn {
		t.Errorf("no disk data: %q (%s)", got, d.nodeLevel(n).mark())
	}
	n.MemUsed, n.CPUAvg, n.Disks = 9*gib, 95, []metrics.DiskUsage{disk}
	if got := d.nodeWhy(msgCtx("en"), n); got != "containers use 90% of its memory, CPU 95% on average" {
		t.Errorf("red memory and CPU over a yellow disk, en: %q", got)
	}
}

// The scheduled digest sends once when the Bot API's reply is long: a reply
// misread as a failure released the claim and re-sent the digest every
// minute for the 3-hour window.
func TestScheduledDigestWithALongReplySendsOnce(t *testing.T) {
	var mu sync.Mutex
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent++
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{"text":"`+strings.Repeat("Сводка ", 2000)+`"}}`)
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 19, 8, 0, 30, 0, time.UTC)
	ch := Channel{ID: 1, OrgID: 7, Enabled: true, DigestEnabled: true, ChatID: "c", DigestMinute: 660, DigestTZ: "Europe/Moscow", DiskThresholdPct: 85, MemThresholdPct: 100}
	st := &fakeStore{digest: []Channel{ch}}
	svc := New(st)
	svc.send = func(ctx context.Context, token, chatID, text string) error {
		return sendTelegramTo(ctx, srv.URL, token, chatID, text)
	}
	d := NewDigest(svc, st, fakeMetrics{}, nil)
	d.now = func() time.Time { return now }
	for i := 0; i < 5; i++ { // five scheduler ticks, a minute apart
		d.tick(context.Background())
		if last, ok := st.claims[1]; ok {
			st.digest[0].DigestLastAt = &last
		}
		now = now.Add(time.Minute)
	}
	if sent != 1 || st.released != 0 {
		t.Fatalf("sent %d times, released %d claims; want once, none", sent, st.released)
	}
}

// componentMetrics records how the digest read its samples.
type componentMetrics struct {
	fakeMetrics
	all  *int
	only *[]string
}

func (c componentMetrics) Since(ctx context.Context, since time.Time) ([]metrics.Sample, error) {
	*c.all++
	return c.fakeMetrics.Since(ctx, since)
}

func (c componentMetrics) SinceFor(_ context.Context, _ time.Time, components []string) ([]metrics.Sample, error) {
	*c.only = append([]string(nil), components...)
	keep := map[string]bool{}
	for _, n := range components {
		keep[n] = true
	}
	var out []metrics.Sample
	for _, s := range c.samples {
		if keep[s.Component] {
			out = append(out, s)
		}
	}
	return out, nil
}

// A digest without the node part reads only the organization's apps'
// samples, not the whole cluster's day — and says exactly what a digest reading
// every sample says; one with the node part still reads everything, because
// the node rows sum every component.
func TestDigestReadsOnlyTheOrgSamplesWithoutInfra(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	st, m := digestFixture(now)
	var all int
	var only []string
	cm := componentMetrics{fakeMetrics: m, all: &all, only: &only}
	ch := Channel{OrgID: 7, Lang: "en", DigestTZ: "UTC", DiskThresholdPct: 85, MemThresholdPct: 100}

	scoped, err := NewDigest(New(st), st, cm, nil).Build(context.Background(), ch, now)
	if err != nil {
		t.Fatal(err)
	}
	if all != 0 {
		t.Fatalf("an app-only digest read the whole cluster (%d times)", all)
	}
	want := []string{"krill-5", "krill-6", "krill-8", "krill-10", "krill-11"} // every app of org 7
	if strings.Join(only, " ") != strings.Join(want, " ") {
		t.Fatalf("components asked for: %v, want %v", only, want)
	}
	full, err := NewDigest(New(st), st, m, nil).Build(context.Background(), ch, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scoped, "\n") != strings.Join(full, "\n") {
		t.Fatalf("the scoped read changed the digest:\nscoped:\n%s\nfull:\n%s", strings.Join(scoped, "\n"), strings.Join(full, "\n"))
	}
	if !strings.Contains(strings.Join(scoped, "\n"), "MiB") {
		t.Fatal("the apps' memory figures are missing")
	}

	ch.NotifyInfra = true
	if _, err := NewDigest(New(st), st, cm, nil).Build(context.Background(), ch, now); err != nil {
		t.Fatal(err)
	}
	if all != 1 {
		t.Fatalf("an infrastructure digest must read every component (Since calls %d)", all)
	}
}

// "от мощности всех ядер (1)" in the msk digest: every count next to a noun
// takes the noun's plural form, for one core and for several, in both
// languages — the node line, the attention block and the app line.
func TestDigestCountsAgree(t *testing.T) {
	const gib = int64(1) << 30
	disk := metrics.DiskUsage{Mount: "/", Total: 100 * gib, Used: 10 * gib, Avail: 90 * gib}
	node := func(name string, ncpu, unlimited int) nodeRow {
		return nodeRow{Name: name, MemTotal: 10 * gib, MemUsed: gib, Limits: gib, NCPU: ncpu, CPUKnown: true,
			CPUAvg: 95, CPUPeak: 99, Unlimited: unlimited, Disks: []metrics.DiskUsage{disk}}
	}
	cases := []struct {
		lang string
		want []string
		not  []string
	}{
		{"ru", []string{
			"CPU: в среднем 95%, пик 99% — от мощности 1 ядра",
			"CPU: в среднем 95%, пик 99% — от мощности всех 4 ядер",
			"нода cp: CPU в среднем 95% от мощности 1 ядра",
			"нода db: CPU в среднем 95% от мощности всех 4 ядер",
			"· 1 контейнер без лимита", "· 5 контейнеров без лимита",
			"p/prod/bot: 1 падение", "bot — ", "· 1 падение", "p/prod/api: 2 падения",
		}, []string{"всех ядер (", "падений 1"}},
		{"en", []string{
			"CPU: 95% on average, 99% at peak — of 1 core",
			"CPU: 95% on average, 99% at peak — of all 4 cores",
			"node cp: CPU at 95% of 1 core on average",
			"node db: CPU at 95% of all 4 cores on average",
			"· 1 container without a limit", "· 5 containers without a limit",
			"p/prod/bot: 1 crash", "· 1 crash", "p/prod/api: 2 crashes",
		}, []string{"all cores (", "crashed 1 times", "1 containers"}},
	}
	for _, c := range cases {
		d := digestData{DiskThreshold: 85, MemThreshold: 100, Lang: c.lang, Infra: true, Loc: time.UTC, Now: time.Now(),
			Nodes:         []nodeRow{node("cp", 1, 1), node("db", 4, 5)},
			RestartsKnown: true,
			Apps: []appRow{
				{Path: "p/prod/bot", Name: "bot", Project: "p", Env: "prod", Peak: 1, Limit: 100, Restarts: 1},
				{Path: "p/prod/api", Name: "api", Project: "p", Env: "prod", Peak: 1, Limit: 100, Restarts: 2},
			}}
		got := strings.Join(renderDigest(d), "\n")
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s digest lacks %q:\n%s", c.lang, w, got)
			}
		}
		for _, w := range c.not {
			if strings.Contains(got, w) {
				t.Errorf("%s digest shows %q:\n%s", c.lang, w, got)
			}
		}
	}
}
