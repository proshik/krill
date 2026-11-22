package notify

import (
	"strings"
	"testing"
	"time"
)

func TestFormatDeployFailed(t *testing.T) {
	ev := Event{Kind: DeployFailed, Project: "proj", Env: "prod", Target: "web", Detail: "crash-loop", Time: time.Unix(0, 0)}
	got := format(ev, "en")
	if got != "<b>🔴 Deploy failed</b>\nweb\nproject proj · environment prod\ncrash-loop" {
		t.Fatalf("unexpected message:\n%s", got)
	}
}

func TestFormatRecoveredHasNoDetailLine(t *testing.T) {
	ev := Event{Kind: AppRecovered, Project: "p", Env: "e", Target: "w", Time: time.Unix(0, 0)}
	got := format(ev, "en")
	if got != "<b>🟢 App recovered</b>\nw\nproject p · environment e" {
		t.Fatalf("unexpected message:\n%s", got)
	}
}

func TestFormatEscapesHTML(t *testing.T) {
	ev := Event{Kind: DeployFailed, Project: "p", Env: "e", Target: "a<b>", Detail: "x & y", Time: time.Unix(0, 0)}
	got := format(ev, "en")
	// User-supplied "<b>" in Target must be escaped; literal "a<b>" must not appear.
	// The format itself uses <b>…</b> for the header (intentional), but "a<b>"
	// injected by the user must become "a&lt;b&gt;".
	if strings.Contains(got, "a<b>") || strings.Contains(got, "x & y") {
		t.Fatalf("HTML not escaped:\n%s", got)
	}
	if !strings.Contains(got, "a&lt;b&gt;") || !strings.Contains(got, "x &amp; y") {
		t.Fatalf("expected escaped entities in output:\n%s", got)
	}
}

func TestFormatMigrateFailed(t *testing.T) {
	got := format(Event{Kind: MigrateFailed, Target: "prod-pg", Detail: "copy volume: boom"}, "en")
	if !strings.Contains(got, "DB migration failed") || !strings.Contains(got, "prod-pg") {
		t.Fatalf("format = %q", got)
	}
	if strings.Contains(got, "//") {
		t.Fatalf("empty path segments leaked: %q", got)
	}
}

// A long error (a build's output in it) is cut so the alert stays under
// Telegram's 4096-character limit instead of being refused.
func TestFormatCapsALongDetail(t *testing.T) {
	msg := format(Event{Kind: DeployFailed, Project: "p", Env: "e", Target: "a", Detail: strings.Repeat("ошибка & ", 2000)}, "ru")
	if n := len([]rune(msg)); n > 4096 {
		t.Fatalf("alert is %d characters", n)
	}
	if !strings.Contains(msg, " …") {
		t.Fatalf("not cut:\n%s", msg)
	}
	// Every "&" comes from escaping the detail's "&": a cut after escaping
	// would leave a bare "&am" that Telegram refuses.
	if strings.Count(msg, "&") != strings.Count(msg, "&amp;") {
		t.Fatalf("a broken entity:\n%s", msg[len(msg)-80:])
	}
}

// An app's name links to its page when the installation has a public https
// address; the name and the address are escaped.
func TestFormatLinksTheApp(t *testing.T) {
	ev := Event{Kind: AppDown, Project: "side", Env: "prod", Target: "demo<app>",
		URL: appURL(linkBase("https://krill.example.com/"), 1, 2, 3, 4)}
	got := format(ev, "ru")
	want := "<b>🔴 Приложение упало</b>\n" +
		`<a href="https://krill.example.com/orgs/1/projects/2/environments/3/apps/4">demo&lt;app&gt;</a>` +
		"\nпроект side · окружение prod"
	if got != want {
		t.Fatalf("message:\n%s\nwant:\n%s", got, want)
	}
	if got := linkName(`https://h/x"><script>`, "a"); got != `<a href="https://h/x&#34;&gt;&lt;script&gt;">a</a>` {
		t.Fatalf("href not escaped: %s", got)
	}
}

func TestLinkBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://krill.example.com":   "https://krill.example.com",
		"https://krill.example.com/":  "https://krill.example.com",
		"https://example.com/krill":   "https://example.com/krill",
		"http://krill.example.com":    "",
		"https://user:pw@example.com": "",
		"https://example.com/?a=b":    "",
		"https://krill.example.com/?": "",
		"https://krill.example.com/#": "",
		"":                            "",
		"krill.example.com":           "",
		"https://":                    "",
	} {
		if got := linkBase(in); got != want {
			t.Errorf("linkBase(%q) = %q, want %q", in, got, want)
		}
	}
	if got := appURL("", 1, 2, 3, 4); got != "" {
		t.Errorf("no base, a link: %q", got)
	}
	if got := appURL("https://h", 1, 0, 3, 4); got != "" {
		t.Errorf("a missing id, a link: %q", got)
	}
}

// A successful deploy says which version went out; a deployment made before
// versions were recorded still names its image.
func TestFormatDeployVersion(t *testing.T) {
	ok := func(d DeployDetail, lang string) string {
		return format(Event{Kind: DeploySucceeded, Project: "side", Env: "prod", Target: "demo-app", Deploy: &d}, lang)
	}
	if got := ok(DeployDetail{Version: "v1.8.3", Image: "demo:v1.8.3", Trigger: "api", Duration: 52 * time.Second}, "ru"); got !=
		"<b>🟢 Деплой успешен</b>\ndemo-app\nпроект side · окружение prod\nверсия v1.8.3 · 52 с · через API" {
		t.Errorf("ru:\n%s", got)
	}
	if got := ok(DeployDetail{Version: "latest (b1139b7c55e2)", Trigger: "webhook", Duration: 185 * time.Second}, "en"); !strings.HasSuffix(got,
		"\nversion latest (b1139b7c55e2) · 3m 5s · via webhook") {
		t.Errorf("en:\n%s", got)
	}
	if got := ok(DeployDetail{Image: "demo@b1139b7c55e2", Trigger: "manual", Duration: time.Hour + 2*time.Minute}, "en"); !strings.HasSuffix(got,
		"\nimage demo@b1139b7c55e2 · 1h 2m · manually") {
		t.Errorf("an old deployment:\n%s", got)
	}
}

// A database server has no project: its message names it alone.
func TestFormatDBEventHasNoPlace(t *testing.T) {
	if got := format(Event{Kind: DBDown, Target: "main-pg", Detail: "no running task"}, "en"); got !=
		"<b>🔴 Database server down</b>\nmain-pg\nno running task" {
		t.Fatalf("message:\n%s", got)
	}
}

// However long a user made a name, the alert stays under Telegram's 4096
// characters.
func TestFormatClipsLongFields(t *testing.T) {
	long := strings.Repeat("p", 4000)
	msg := format(Event{Kind: DeploySucceeded, Project: long, Env: long, Target: long,
		Deploy: &DeployDetail{Version: strings.Repeat("b", 250) + "@1a09ec0", Trigger: "api", Duration: time.Second}}, "en")
	if n := len([]rune(msg)); n > 4096 {
		t.Fatalf("alert is %d characters", n)
	}
	failed := format(Event{Kind: DeployFailed, Project: long, Env: long, Target: long, Detail: strings.Repeat("e ", 3000)}, "en")
	if n := len([]rune(failed)); n > 4096 {
		t.Fatalf("failure alert is %d characters", n)
	}
}

// A long branch keeps its commit when clipped: it is what tells two
// deploys apart.
func TestClipVersionKeepsTheCommit(t *testing.T) {
	v := strings.Repeat("b", 250) + "@1a09ec0"
	got := clipVersion(v)
	if n := len([]rune(got)); n != fieldMax || !strings.HasSuffix(got, "…@1a09ec0") {
		t.Fatalf("clipVersion = %q (%d)", got, n)
	}
	if got := clipVersion("latest (b1139b7c55e2)"); got != "latest (b1139b7c55e2)" {
		t.Fatalf("a short version changed: %q", got)
	}
}
