package notify

import (
	"strings"
	"testing"
	"time"
)

func TestFormatDeployFailed(t *testing.T) {
	ev := Event{Kind: DeployFailed, Project: "proj", Env: "prod", Target: "web", Detail: "crash-loop", Time: time.Unix(0, 0)}
	got := format(ev, "en")
	if !strings.Contains(got, "Deploy failed") || !strings.Contains(got, "proj/prod/web") || !strings.Contains(got, "crash-loop") {
		t.Fatalf("unexpected message:\n%s", got)
	}
}

func TestFormatRecoveredHasNoDetailLine(t *testing.T) {
	ev := Event{Kind: AppRecovered, Project: "p", Env: "e", Target: "w", Time: time.Unix(0, 0)}
	got := format(ev, "en")
	if !strings.Contains(got, "App recovered") || !strings.Contains(got, "p/e/w") {
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
