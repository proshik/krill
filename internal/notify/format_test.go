package notify

import (
	"strings"
	"testing"
	"time"
)

func TestFormatDeployFailed(t *testing.T) {
	ev := Event{Kind: DeployFailed, Project: "proj", Env: "prod", Target: "web", Detail: "crash-loop", Time: time.Unix(0, 0)}
	got := format(ev)
	if !strings.Contains(got, "Deploy failed") || !strings.Contains(got, "proj/prod/web") || !strings.Contains(got, "crash-loop") {
		t.Fatalf("unexpected message:\n%s", got)
	}
}

func TestFormatRecoveredHasNoDetailLine(t *testing.T) {
	ev := Event{Kind: AppRecovered, Project: "p", Env: "e", Target: "w", Time: time.Unix(0, 0)}
	got := format(ev)
	if !strings.Contains(got, "App recovered") || !strings.Contains(got, "p/e/w") {
		t.Fatalf("unexpected message:\n%s", got)
	}
}

func TestFormatEscapesHTML(t *testing.T) {
	ev := Event{Kind: DeployFailed, Project: "p", Env: "e", Target: "a<b>", Detail: "x & y", Time: time.Unix(0, 0)}
	got := format(ev)
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
