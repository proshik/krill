package observability

import (
	"strings"
	"testing"
)

// benignAlloyDockerTailerError is the msg text of the one error line
// TestNodeAgentShipsMetricsAndLogs tolerates. loki.source.docker's tailer
// discovers containers and then inspects them in two separate steps; when
// this package's integration tests run alongside internal/dbservice's and
// internal/docker's (as `make test-integration` does for the whole repo),
// those packages create and remove containers on the same shared Docker host
// concurrently, so a container can disappear between the agent's discovery
// and its inspect. Alloy logs that as a "could not inspect container info"
// error from loki.source.docker. It is not a defect in the config this
// package renders — the identical race happens in production whenever a
// container exits while Alloy is mid-inspect — so this one line must not
// fail a test that otherwise wants a clean agent log.
const benignAlloyDockerTailerError = `msg="could not inspect container info"`

// nonBenignErrorLines returns every "level=error" line in out except the
// benign loki.source.docker tailer race described above (benignAlloyDockerTailerError).
// An empty result means the agent's log is clean of anything else.
func nonBenignErrorLines(out []byte) []string {
	var bad []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "level=error") {
			continue
		}
		if strings.Contains(line, benignAlloyDockerTailerError) && strings.Contains(line, "loki.source.docker") {
			continue
		}
		bad = append(bad, line)
	}
	return bad
}

func TestNonBenignErrorLines(t *testing.T) {
	benign := `level=error ts=2026-09-18T00:00:00Z msg="could not inspect container info" component_path=/ component_id=loki.source.docker.node component=tailer container=docker/abc123`
	real := `level=error ts=2026-09-18T00:00:01Z msg="failed to send batch, retrying" component_id=loki.write.default`
	info := `level=info ts=2026-09-18T00:00:02Z msg="now listening for http traffic" addr=0.0.0.0:12345`
	out := []byte(strings.Join([]string{benign, real, info}, "\n"))

	bad := nonBenignErrorLines(out)
	if len(bad) != 1 || bad[0] != real {
		t.Fatalf("nonBenignErrorLines = %#v, want only the real error line %q", bad, real)
	}
}
