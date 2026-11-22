package notify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
)

// Deploys collapse to one line per app: how many succeeded, the version it
// runs now and when it went out; every failed deploy keeps a line of its own
// with its error. App names link to their pages when there is a public
// https address.
func TestDigestCollapsesDeploysPerApp(t *testing.T) {
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	dep := func(app int64, name, status, version, image string, ago time.Duration, errMsg string) DeploymentInfo {
		return DeploymentInfo{AppID: app, OrgID: 7, ProjectID: 2, EnvID: 3, Project: "side", Env: "prod", App: name,
			Status: status, Trigger: "api", Version: version, ImageTag: image, Error: errMsg, FinishedAt: now.Add(-ago)}
	}
	st := &fakeStore{orgDeploys: []DeploymentInfo{
		dep(4, "demo-app", "done", "v1.8.1", "", 20*time.Hour, ""),
		dep(4, "demo-app", "done", "v1.8.2", "", 10*time.Hour, ""),
		dep(4, "demo-app", "error", "v1.8.3-rc", "", 9*time.Hour, "service crash-looping"),
		dep(4, "demo-app", "done", "v1.8.3", "", 7*time.Hour+17*time.Minute, ""),
		dep(4, "demo-app", "done", "v1.8.3", "", 6*time.Hour, ""),
		dep(4, "demo-app", "done", "v1.8.3", "", 5*time.Hour+17*time.Minute, ""),
		dep(9, "worker", "done", "", "ghcr.io/acme/worker@sha256:b1139b7c55e2aaaa", time.Hour, ""),
		dep(9, "worker", "done", "main@1a09ec0", "", 30*time.Minute, ""),
		{AppID: 12, OrgID: 7, ProjectID: 5, EnvID: 6, Project: "tools", Env: "dev", App: "cron", Status: "error", FinishedAt: now.Add(-2 * time.Hour)},
	}}
	svc := New(st)
	svc.SetPublicURL(func(context.Context) string { return "https://krill.example.com" })
	d := NewDigest(svc, st, fakeMetrics{}, nil)
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, Lang: "ru", DigestTZ: "Europe/Moscow"}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(parts, "\n")
	t.Logf("digest:\n%s", got)
	link := `<a href="https://krill.example.com/orgs/7/projects/2/environments/3/apps/4">demo-app</a>`
	for _, want := range []string{
		"🚀 <b>Деплои</b>: 9, успешно 7, с ошибкой 2\n<b>side · prod</b>\n",
		"🟢 " + link + " — 5 деплоев, сейчас v1.8.3 (05:43)\n🔴 " + link + " — v1.8.3-rc · 02:00 · service crash-looping\n",
		"— 2 деплоя, сейчас main@1a09ec0 (10:30)",
		"<b>tools · dev</b>\n🔴 <a href=\"https://krill.example.com/orgs/7/projects/5/environments/6/apps/12\">cron</a> — 09:00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q", want)
		}
	}
	if strings.Count(got, "v1.8.1") != 0 || strings.Count(got, "v1.8.2") != 0 {
		t.Errorf("collapsed deploys still list the earlier versions")
	}

	// Without a public https address the names are plain text.
	plain := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, err = plain.Build(context.Background(), Channel{OrgID: 7, Lang: "en", DigestTZ: "UTC"}, now)
	if err != nil {
		t.Fatal(err)
	}
	got = strings.Join(parts, "\n")
	if strings.Contains(got, "<a ") || !strings.Contains(got, "🟢 demo-app — 5 deploys, now v1.8.3 (02:43)") || !strings.Contains(got, "🟢 worker — 2 deploys") {
		t.Errorf("plain digest:\n%s", got)
	}
}

// Success lines beyond the cap are counted; a failed deploy is listed
// whatever sorts before it.
func TestDigestCapsDeployLines(t *testing.T) {
	now := time.Now()
	st := &fakeStore{}
	for i := 0; i < 30; i++ {
		st.orgDeploys = append(st.orgDeploys, DeploymentInfo{AppID: int64(i + 1), Project: "p", Env: "e", App: fmt.Sprintf("app%02d", i), Status: "done", Version: "v1", FinishedAt: now})
	}
	st.orgDeploys = append(st.orgDeploys,
		DeploymentInfo{AppID: 99, Project: "p", Env: "e", App: "zz-last", Status: "error", Version: "v2", Error: "boom", FinishedAt: now},
		DeploymentInfo{AppID: 98, Project: "z", Env: "e", App: "other", Status: "error", Error: "bang", FinishedAt: now})
	d := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, _ := d.Build(context.Background(), Channel{OrgID: 7, Lang: "en", DigestTZ: "UTC"}, now)
	got := strings.Join(parts, "\n")
	if strings.Count(got, "1 deploy, now v1") != digestListMax || !strings.Contains(got, "…and 10 more") {
		t.Fatalf("deploy lines not capped:\n%s", got)
	}
	for _, want := range []string{"🔴 zz-last — v2 · ", " · boom", "<b>z · e</b>\n🔴 other — ", " · bang"} {
		if !strings.Contains(got, want) {
			t.Errorf("a failed deploy is crowded out: %q missing\n%s", want, got)
		}
	}
}

// A deploy Krill made itself is not counted, but when it is the newest
// success its version is what the app was last deployed with.
func TestDigestSystemDeployMovesTheVersion(t *testing.T) {
	now := time.Now()
	st := &fakeStore{orgDeploys: []DeploymentInfo{
		{AppID: 1, Project: "p", Env: "e", App: "web", Status: "done", Trigger: "manual", Version: "main@1a09ec0", FinishedAt: now.Add(-2 * time.Hour)},
		{AppID: 1, Project: "p", Env: "e", App: "web", Status: "done", Trigger: "system", Version: "main@9f2c4d1", FinishedAt: now.Add(-time.Hour)},
		{AppID: 2, Project: "p", Env: "e", App: "quiet", Status: "done", Trigger: "system", Version: "v1.0.0", FinishedAt: now},
	}}
	d := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, _ := d.Build(context.Background(), Channel{OrgID: 7, Lang: "en", DigestTZ: "UTC"}, now)
	got := strings.Join(parts, "\n")
	if !strings.Contains(got, "🚀 <b>Deploys</b>: 1, all succeeded") || !strings.Contains(got, "🟢 web — 1 deploy, now main@9f2c4d1") || strings.Contains(got, "quiet") {
		t.Fatalf("system deploys:\n%s", got)
	}
}

// Apps without fresh data stay in their project and environment, linked.
func TestDigestIdleAppsAreGroupedAndLinked(t *testing.T) {
	now := time.Now()
	st := &fakeStore{orgApps: []WatchedApp{{AppID: 4, OrgID: 7, ProjectID: 2, EnvID: 3, Project: "side", Env: "prod", Name: "demo-app"}}}
	svc := New(st)
	svc.SetPublicURL(func(context.Context) string { return "https://krill.example.com" })
	sw := fakeSwarm{fails: map[string]int{}, states: map[string]docker.ServiceState{"krill-4": {Found: true, Desired: 1}}}
	d := NewDigest(svc, st, fakeMetrics{}, sw)
	parts, _ := d.Build(context.Background(), Channel{OrgID: 7, Lang: "en", DigestTZ: "UTC"}, now)
	got := strings.Join(parts, "\n")
	want := "<b>side · prod</b>\n🔴 Not running: <a href=\"https://krill.example.com/orgs/7/projects/2/environments/3/apps/4\">demo-app</a>"
	if !strings.Contains(got, want) {
		t.Fatalf("idle app:\n%s\nwant:\n%s", got, want)
	}
}

// Backups are named by what was backed up and grouped by project and
// environment.
func TestDigestGroupsBackups(t *testing.T) {
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	st := &fakeStore{runs: []BackupRun{
		{OrgID: 7, Kind: BackupKindDatabase, Project: "side", Env: "prod", Target: "orders", Status: "ok", SizeBytes: 336 << 20, FinishedAt: now.Add(-6 * time.Hour)},
		{OrgID: 7, Kind: BackupKindVolume, Project: "side", Env: "prod", Target: "demo-app / data", Status: "ok", SizeBytes: 95, FinishedAt: now.Add(-6 * time.Hour)},
		{OrgID: 7, Kind: BackupKindDatabase, Project: "alpha", Env: "dev", Target: "scratch", Status: "ok", SizeBytes: 1 << 20, FinishedAt: now.Add(-5 * time.Hour)},
	}}
	d := NewDigest(New(st), st, fakeMetrics{}, nil)
	parts, _ := d.Build(context.Background(), Channel{OrgID: 7, Lang: "ru", DigestTZ: "UTC"}, now)
	got := strings.Join(parts, "\n")
	want := "💾 <b>Бэкапы</b>: 3, все успешны\n<b>alpha · dev</b>\n🟢 база scratch · 1.0 MiB · 03:00\n" +
		"<b>side · prod</b>\n🟢 база orders · 336 MiB · 02:00\n🟢 том demo-app / data · 95 B · 02:00"
	if !strings.Contains(got, want) {
		t.Fatalf("backups:\n%s\nwant:\n%s", got, want)
	}
}

func TestPluralForm(t *testing.T) {
	for n, want := range map[int]string{1: "one", 2: "few", 4: "few", 5: "many", 11: "many", 12: "many", 21: "one", 22: "few", 25: "many", 111: "many", 0: "many"} {
		if got := pluralForm("ru", n); got != want {
			t.Errorf("ru %d: %s, want %s", n, got, want)
		}
	}
	if pluralForm("en", 1) != "one" || pluralForm("en", 2) != "many" {
		t.Error("en plural")
	}
}

// openTags counts the unclosed occurrences of a tag in an HTML part.
func balanced(s string) bool {
	for _, tag := range []string{"blockquote", "a", "b"} {
		open := strings.Count(s, "<"+tag+">") + strings.Count(s, "<"+tag+" ")
		if open != strings.Count(s, "</"+tag+">") {
			return false
		}
	}
	return true
}

// A part boundary inside the collapsed node details closes the quote at the
// end of one part and opens it again at the start of the next: Telegram
// refuses a message with an unclosed tag.
func TestSplitPartsKeepsTheQuoteBalanced(t *testing.T) {
	lines := []string{"<b>head</b>", "", quoteOpen + "<b>n1</b>"}
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf("line %02d of the details", i))
	}
	lines[len(lines)-1] += quoteClose
	lines = append(lines, "", "<b>after</b>")
	parts := splitParts(lines, 200)
	if len(parts) < 3 {
		t.Fatalf("want the quote split over several parts, got %d", len(parts))
	}
	var rejoined []string
	for i, p := range parts {
		if n := len([]rune(p)); n > 200 {
			t.Errorf("part %d is %d characters", i, n)
		}
		if !balanced(p) {
			t.Errorf("part %d is not balanced:\n%s", i, p)
		}
		rejoined = append(rejoined, p)
	}
	all := strings.Join(rejoined, "\n")
	for i := 0; i < 40; i++ {
		if !strings.Contains(all, fmt.Sprintf("line %02d of the details", i)) {
			t.Errorf("line %d lost", i)
		}
	}
	if !strings.HasSuffix(parts[len(parts)-1], "<b>after</b>") || strings.Contains(parts[len(parts)-1], quoteOpen+"<b>after") {
		t.Errorf("the line after the quote was put back into it:\n%s", parts[len(parts)-1])
	}
}

// A whole digest whose node details run past a part keeps every part
// balanced, and the details stay complete.
func TestDigestWithManyNodesSplitsBalanced(t *testing.T) {
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	const gib = int64(1) << 30
	var caps []metrics.NodeCapacity
	var disks []metrics.NodeDisk
	for i := 0; i < 40; i++ {
		node := fmt.Sprintf("worker-%02d", i)
		caps = append(caps, metrics.NodeCapacity{Node: node, NCPU: 2, MemTotal: 4 * gib, SampledAt: now.Add(-time.Minute)})
		disks = append(disks,
			metrics.NodeDisk{Node: node, DiskUsage: metrics.DiskUsage{Mount: "/", Total: 80 * gib, Used: 20 * gib, Avail: 60 * gib}},
			metrics.NodeDisk{Node: node, DiskUsage: metrics.DiskUsage{Mount: "/var/lib/docker", Total: 200 * gib, Used: 150 * gib, Avail: 50 * gib}})
	}
	st := &fakeStore{}
	d := NewDigest(New(st), st, fakeMetrics{caps: caps, disks: disks}, nil)
	parts, err := d.Build(context.Background(), Channel{OrgID: 7, Lang: "ru", DigestTZ: "UTC", NotifyInfra: true, DiskThresholdPct: 85, MemThresholdPct: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 2 {
		t.Fatalf("want several parts, got %d", len(parts))
	}
	quoted := 0
	for i, p := range parts {
		if n := len([]rune(p)); n > digestPartLimit {
			t.Errorf("part %d is %d characters", i, n)
		}
		if !balanced(p) {
			t.Errorf("part %d is not balanced:\n%s", i, p)
		}
		if strings.Contains(p, quoteOpen) {
			quoted++
		}
	}
	if quoted < 2 {
		t.Errorf("the details did not span parts (%d parts carry the quote)", quoted)
	}
	all := strings.Join(parts, "\n")
	if c := strings.Count(all, "🟡 диск /var/lib/docker: занято 75%"); c != 40 {
		t.Errorf("a yellow detail line lost its mark or went missing: %d of 40", c)
	}
}

// A line cut at the part limit never ends inside a tag or an entity and
// closes the inline tags it left open.
func TestCutLineClosesTags(t *testing.T) {
	line := []rune(`🟢 <a href="https://h/x">` + strings.Repeat("n", 30) + `</a> — <b>x &amp; y</b>`)
	for limit := 1; limit < len(line); limit++ {
		got := cutLine(line, limit)
		if !balanced(got) {
			t.Fatalf("limit %d: %q", limit, got)
		}
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("limit %d: a broken entity in %q", limit, got)
		}
	}
	for limit := 1; limit < len(line); limit++ {
		if n := len([]rune(cutLine(line, limit))); n > limit {
			t.Fatalf("limit %d: %d characters with the closing tags", limit, n)
		}
	}
	if got := cutLine([]rune("abc&amp;def"), 6); got != "abc" {
		t.Errorf("cut inside an entity: %q", got)
	}
}
