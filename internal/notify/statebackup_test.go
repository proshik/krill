package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeState struct {
	st       StateStatus
	err      error
	prunedAt time.Time
}

func (f *fakeState) StateBackupStatus(context.Context, time.Time) (StateStatus, error) {
	return f.st, f.err
}
func (f *fakeState) PruneStateRuns(_ context.Context, before time.Time) error {
	f.prunedAt = before
	return nil
}

func TestStateBackupFailedInfraOnly(t *testing.T) {
	st := &fakeStore{infra: []Channel{
		{ID: 1, OrgID: 1, Enabled: true, ChatID: "ops", NotifyInfra: true, Lang: "en"},
		{ID: 2, OrgID: 2, Enabled: true, ChatID: "org", NotifyInfra: false, NotifyBackup: true, Lang: "en"},
	}}
	send := &flakySend{}
	svc := New(st)
	svc.send = send.fn
	svc.SetInstanceName(func(context.Context) string { return "krill.example" })
	svc.stateBackupFailed(context.Background(), "pre_update", "v0.9.3", "v0.9.4", "s3 refused")
	if send.count() != 1 {
		t.Fatalf("sends: %v", send.all())
	}
	got := send.all()[0]
	for _, want := range []string{"Krill state backup failed", "krill.example", "before an update", "v0.9.3 → v0.9.4", "s3 refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("alert lacks %q:\n%s", want, got)
		}
	}
}

func TestStateBackupFailedSkipsBrokenChannel(t *testing.T) {
	st := &fakeStore{
		infra:    []Channel{{ID: 1, Enabled: true, ChatID: "ops", NotifyInfra: true, Lang: "en"}},
		infraErr: errors.New("channel 9: token undecryptable"),
	}
	send := &flakySend{}
	svc := New(st)
	svc.send = send.fn
	svc.stateBackupFailed(context.Background(), "schedule", "", "", "boom")
	if send.count() != 1 {
		t.Fatalf("the working channel must still be alerted: %v", send.all())
	}
}

func TestStateBackupFailedDetailCapped(t *testing.T) {
	st := &fakeStore{infra: []Channel{{ID: 1, Enabled: true, ChatID: "ops", NotifyInfra: true, Lang: "ru"}}}
	send := &flakySend{}
	svc := New(st)
	svc.send = send.fn
	svc.stateBackupFailed(context.Background(), "schedule", "", "", strings.Repeat("q", 5000))
	got := send.all()[0]
	if n := strings.Count(got, "q"); n > detailMax {
		t.Fatalf("detail not capped: %d runes", n)
	}
	if !strings.Contains(got, "по расписанию") {
		t.Errorf("ru trigger label missing:\n%s", got)
	}
}

func TestStateOverdue(t *testing.T) {
	old := cronLoc
	cronLoc = time.UTC
	defer func() { cronLoc = old }()
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) }
	cases := []struct {
		name             string
		sched            string
		last, since, now time.Time
		want             bool
	}{
		{"daily late", "0 3 * * *", at(19, 3, 1), time.Time{}, at(20, 4, 30), true},
		{"daily inside grace", "0 3 * * *", at(19, 3, 1), time.Time{}, at(20, 3, 30), false},
		{"weekly", "0 3 * * 0", at(17, 3, 0), time.Time{}, at(20, 4, 0), false},
		{"on demand", "", at(1, 3, 0), time.Time{}, at(20, 4, 0), false},
		{"weekly 8 days", "0 3 * * 0", at(12, 3, 0), time.Time{}, at(20, 4, 0), true},
		{"monthly late", "0 3 1 * *", at(1, 3, 0).AddDate(0, -1, 0), time.Time{}, at(20, 4, 0), true},
		{"monthly fine", "0 3 1 * *", at(1, 3, 0), time.Time{}, at(20, 4, 0), false},
		{"both zero", "0 3 * * *", time.Time{}, time.Time{}, at(20, 4, 0), false},
		{"never, saved 2 days ago", "0 3 * * *", time.Time{}, at(18, 12, 0), at(20, 12, 0), true},
	}
	for _, c := range cases {
		if got := stateOverdue(c.sched, c.last, c.since, c.now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func stateDigest(t *testing.T, st StateStatus, infra bool, lang string) string {
	t.Helper()
	old := cronLoc
	cronLoc = time.UTC
	defer func() { cronLoc = old }()
	fs := &fakeStore{}
	d := NewDigest(New(fs), fs, fakeMetrics{}, nil)
	d.SetStateBackups(&fakeState{st: st})
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", Lang: lang, NotifyInfra: infra}, now)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, "\n")
}

func TestDigestStateBlock(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 20, h, 0, 0, 0, time.UTC) }
	ok := func(h int) StateRun { return StateRun{Trigger: "schedule", Status: "ok", Size: 2048, At: at(h)} }
	bad := func(h int, tr string) StateRun {
		return StateRun{Trigger: tr, Status: "error", Error: "boom", From: "v0.9.3", To: "v0.9.4", At: at(h)}
	}
	base := StateStatus{Configured: true, Schedule: "0 3 * * *", Since: at(0).AddDate(0, 0, -30), LastSuccess: at(3)}
	with := func(f func(*StateStatus)) StateStatus { s := base; f(&s); return s }

	cases := []struct {
		name string
		st   StateStatus
		en   []string
		ru   []string
		not  []string
	}{
		{"off", StateStatus{}, []string{"🟡 not backed up: state backup is not set up"}, []string{"🟡 нет бэкапа: бэкап состояния не настроен"}, nil},
		{"paused", with(func(s *StateStatus) { s.Paused = true }), []string{"🟡 not backed up: state backup is paused"}, []string{"🟡 нет бэкапа: бэкап состояния на паузе"}, nil},
		{"ok", with(func(s *StateStatus) { s.Runs = []StateRun{ok(3), ok(2), ok(1)} }),
			[]string{"🟢 3 backups, the last one 2.0 KiB at 03:00"}, []string{"🟢 3 бэкапа, последний — 2.0 KiB в 03:00"}, nil},
		{"failed", with(func(s *StateStatus) {
			s.Runs = []StateRun{bad(1, "schedule"), bad(2, "schedule"), bad(3, "pre_update"), bad(4, "schedule"), bad(5, "schedule")}
		}), []string{"🔴 backup failed: 01:00 · scheduled · boom", "before an update · v0.9.3 → v0.9.4", "🔴 …and 2 more"},
			[]string{"🔴 бэкап не удался: 01:00 · по расписанию · boom", "🔴 …и ещё 2"}, nil},
		{"overdue", with(func(s *StateStatus) { s.LastSuccess = at(3).AddDate(0, 0, -2) }),
			[]string{"🟡 a scheduled backup is overdue, the last one was at Nov 18 03:00"}, []string{"🟡 бэкап по расписанию просрочен, последний: 18.11 03:00"}, nil},
		{"ok but overdue", with(func(s *StateStatus) {
			s.LastSuccess = at(3).AddDate(0, 0, -2)
			s.Runs = []StateRun{{Trigger: "manual", Status: "ok", Size: 2048, At: at(1)}}
		}), []string{"🟢 1 backup, the last one", "🟡 a scheduled backup is overdue, the last one was at Nov 18 03:00"}, nil, nil},
		{"nothing due", base, []string{"\nlast backup 03:00"}, []string{"\nпоследний бэкап: 03:00"}, []string{"🟢 last", "🟡 last"}},
		{"on demand", with(func(s *StateStatus) { s.Schedule = ""; s.LastSuccess = time.Time{} }),
			[]string{"\non demand only, last backup never"}, []string{"\nтолько по запросу, последний бэкап: не было"}, []string{"🟡 on demand"}},
	}
	for _, c := range cases {
		en := stateDigest(t, c.st, true, "en")
		ru := stateDigest(t, c.st, true, "ru")
		for _, w := range c.en {
			if !strings.Contains(en, w) {
				t.Errorf("%s en lacks %q:\n%s", c.name, w, en)
			}
		}
		for _, w := range c.ru {
			if !strings.Contains(ru, w) {
				t.Errorf("%s ru lacks %q:\n%s", c.name, w, ru)
			}
		}
		for _, w := range c.not {
			if strings.Contains(en, w) {
				t.Errorf("%s en has %q:\n%s", c.name, w, en)
			}
		}
		if !strings.Contains(en, "<b>Krill state</b>") || !strings.Contains(ru, "<b>Состояние Krill</b>") {
			t.Errorf("%s: heading missing", c.name)
		}
	}
}

func TestDigestStateBlockReaderError(t *testing.T) {
	fs := &fakeStore{}
	d := NewDigest(New(fs), fs, fakeMetrics{}, nil)
	d.SetStateBackups(&fakeState{err: errors.New("db down")})
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", Lang: "en", NotifyInfra: true}, time.Now())
	if err != nil {
		t.Fatalf("a reader error failed the digest: %v", err)
	}
	if got := strings.Join(parts, "\n"); !strings.Contains(got, "<b>Krill state</b>\nno data") {
		t.Fatalf("no_data under the heading expected:\n%s", got)
	}
}

func TestDigestStateBlockNotForOrgChannels(t *testing.T) {
	got := stateDigest(t, StateStatus{}, false, "en")
	if strings.Contains(got, "Krill state") {
		t.Fatalf("org channel got the state block:\n%s", got)
	}
}

func TestDigestPrunesStateRuns(t *testing.T) {
	fs := &fakeStore{}
	fst := &fakeState{}
	d := NewDigest(New(fs), fs, fakeMetrics{}, nil)
	d.SetStateBackups(fst)
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	d.prune(context.Background())
	if want := now.Add(-backupRunsKeep); !fst.prunedAt.Equal(want) {
		t.Fatalf("pruned before %v, want %v", fst.prunedAt, want)
	}
}

func TestDigestStateBlockCanSplitMessage(t *testing.T) {
	fs := &fakeStore{}
	now := time.Now()
	for i := 0; i < 20; i++ {
		fs.orgDeploys = append(fs.orgDeploys, DeploymentInfo{Project: "p", Env: "e", App: "a", Status: "error", Error: strings.Repeat("e", 200), FinishedAt: now})
		fs.runs = append(fs.runs, BackupRun{OrgID: 7, Kind: "database", Project: "p", Env: "e", Target: "t", Status: "error", Error: strings.Repeat("f", 200), FinishedAt: now})
	}
	d := NewDigest(New(fs), fs, fakeMetrics{}, nil)
	d.SetStateBackups(&fakeState{st: StateStatus{}})
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, DigestTZ: "UTC", Lang: "en", NotifyInfra: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected a split, got %d part(s)", len(parts))
	}
	for _, p := range parts {
		if len([]rune(p)) > digestPartLimit {
			t.Fatalf("part over the limit: %d", len([]rune(p)))
		}
	}
	if !strings.Contains(strings.Join(parts, "\n"), "not backed up") {
		t.Fatal("state line lost in the split")
	}
}

func TestStateOverdueUsesSchedulerZone(t *testing.T) {
	old := cronLoc
	cronLoc = time.FixedZone("UTC-5", -5*3600)
	defer func() { cronLoc = old }()
	last := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC) // 03:00 local
	if stateOverdue("0 3 * * *", last, time.Time{}, time.Date(2026, 11, 20, 4, 30, 0, 0, time.UTC)) {
		t.Error("overdue before the next 03:00 local")
	}
	if !stateOverdue("0 3 * * *", last, time.Time{}, time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)) {
		t.Error("not overdue after 03:00 local + grace")
	}
}

func TestDigestStateUnknownStatusNotGreen(t *testing.T) {
	got := stateDigest(t, StateStatus{Configured: true, Schedule: "0 3 * * *", Since: time.Now(), Runs: []StateRun{{Status: "running", At: time.Now()}}}, true, "en")
	if strings.Contains(got, "🟢 1 backups") {
		t.Fatalf("a non-ok run shown green:\n%s", got)
	}
}
