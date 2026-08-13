package server

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// The log stream's scanner error was never checked: a line over the 1 MiB
// buffer (bufio.ErrTooLong) or a broken read ended the loop and the socket
// closed with StatusNormalClosure, so the viewer showed a stream that had
// simply "ended". The user watches a dead panel believing it is live.
func TestScanEndNotice(t *testing.T) {
	if _, ok := scanEndNotice(nil); ok {
		t.Error("a clean end of stream must not produce a notice")
	}
	line, ok := scanEndNotice(bufio.ErrTooLong)
	if !ok {
		t.Fatal("an oversized log line produced no notice")
	}
	if line.Level != "error" {
		t.Errorf("notice level = %q, want error", line.Level)
	}
	if line.Msg == "" {
		t.Error("notice has no message explaining the break")
	}
	if other, ok := scanEndNotice(errors.New("connection reset")); !ok || !strings.Contains(other.Msg, "connection reset") {
		t.Errorf("a read failure must surface its cause, got %q (ok=%v)", other.Msg, ok)
	}
}
