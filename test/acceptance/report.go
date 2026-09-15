//go:build acceptance

package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Evidence collects what one step did — commands, key outputs, timings —
// and appends it to <workdir>/report.md as one block when the step ends.
type Evidence struct {
	t     testing.TB
	path  string
	title string
	start time.Time

	mu    sync.Mutex
	lines []string
}

// NewEvidence starts an evidence block for a step; the block is written by a
// t.Cleanup, pass or fail.
func NewEvidence(t testing.TB, cfg Config, title string) *Evidence {
	e := &Evidence{t: t, path: filepath.Join(cfg.Workdir, "report.md"), title: title, start: time.Now()}
	t.Cleanup(e.flush)
	return e
}

// Note records one line of evidence and logs it.
func (e *Evidence) Note(format string, args ...any) {
	line := redact(fmt.Sprintf(format, args...))
	e.t.Log(line)
	e.mu.Lock()
	e.lines = append(e.lines, line)
	e.mu.Unlock()
}

// Output records a command and its (trimmed, bounded) output.
func (e *Evidence) Output(cmd, out string) {
	cmd, out = redact(cmd), redact(strings.TrimSpace(out))
	if len(out) > 4000 {
		out = out[:2000] + "\n...[truncated]...\n" + out[len(out)-2000:]
	}
	e.mu.Lock()
	e.lines = append(e.lines, "$ "+cmd+"\n"+out)
	e.mu.Unlock()
}

func (e *Evidence) flush() {
	status := "PASS"
	if e.t.Failed() {
		status = "FAIL"
	} else if e.t.Skipped() {
		status = "SKIP"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## %s — %s (%s, started %s)\n\n```\n", e.title, status,
		time.Since(e.start).Round(time.Second), e.start.Format(time.RFC3339))
	e.mu.Lock()
	for _, l := range e.lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	e.mu.Unlock()
	b.WriteString("```\n")
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Logf("evidence: opening %s: %v", e.path, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		e.t.Logf("evidence: writing %s: %v", e.path, err)
	}
}
