package templates

import (
	"bytes"
	"context"
	"regexp"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// Connect follows the running replicas, not the status: a first deploy reads
// "deploying" with no container yet, and a stopped app may run again.
func TestTerminalConnectFollowsReplicas(t *testing.T) {
	render := func(status string, running int) string {
		c := AppCtx{Org: db.Organization{ID: 1}, Project: db.Project{ID: 2}, Env: db.Environment{ID: 3}, App: db.Application{ID: 4, Status: status}, Role: "owner", Running: running}
		var buf bytes.Buffer
		if err := terminalTab(c).Render(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	disabled := regexp.MustCompile(`<button[^>]*id="term-connect"[^>]*\sdisabled`)
	for _, tc := range []struct {
		status  string
		running int
		want    bool
	}{
		{"deploying", 0, true}, // first deploy, no container yet
		{"idle", 1, false},     // started elsewhere since the status was read
		{"running", 1, false},
		{"not_deployed", -1, true}, // Swarm did not answer: fall back to the status
		{"running", -1, false},
	} {
		if got := disabled.MatchString(render(tc.status, tc.running)); got != tc.want {
			t.Errorf("status %s, running %d: disabled = %v, want %v", tc.status, tc.running, got, tc.want)
		}
	}
	var buf bytes.Buffer
	if err := AppStateBadge("running", "/s", 2, false).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`data-running="2"`).MatchString(buf.String()) {
		t.Errorf("the badge must carry the running replicas for the terminal: %s", buf.String())
	}
}
