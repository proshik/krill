//go:build acceptance

package acceptance

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// The log sink is a copy of everything the harness logs, kept in the workdir
// so a test can scan it for secrets at the end (go test's own output is not
// readable from inside the test).
var (
	sinkMu sync.Mutex
	sink   *os.File
)

// openLogSink starts copying the harness log to path (truncated).
func openLogSink(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	sinkMu.Lock()
	if sink != nil {
		sink.Close()
	}
	sink = f
	sinkMu.Unlock()
	return nil
}

// logf logs through t and appends the same line to the sink.
func logf(t testing.TB, format string, args ...any) {
	t.Helper()
	line := fmt.Sprintf(format, args...)
	t.Log(line)
	sinkMu.Lock()
	defer sinkMu.Unlock()
	if sink != nil {
		fmt.Fprintln(sink, line)
	}
}
