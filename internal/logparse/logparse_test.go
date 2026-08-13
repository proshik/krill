package logparse

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func TestParseLogLine(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantT   string
		wantLvl string
		wantMsg string
	}{
		{
			name:    "docker ts + logfmt info",
			raw:     "2026-06-07T19:31:52.307218Z time=2026-06-07T19:31:52.307+03:00 level=INFO msg=database_connected",
			wantT:   "2026-06-07T19:31:52.307218Z",
			wantLvl: "info",
			wantMsg: "time=2026-06-07T19:31:52.307+03:00 level=INFO msg=database_connected",
		},
		{
			name:    "docker ts + quarkus warn",
			raw:     "2026-06-07T20:53:24.434Z 2026-06-07 20:53:24,434 WARN  [io.quarkus] (main) something",
			wantT:   "2026-06-07T20:53:24.434Z",
			wantLvl: "warn",
			wantMsg: "2026-06-07 20:53:24,434 WARN  [io.quarkus] (main) something",
		},
		{
			name:    "logfmt warning normalizes to warn",
			raw:     "2026-06-07T19:31:52.000Z level=warning msg=slow",
			wantT:   "2026-06-07T19:31:52.000Z",
			wantLvl: "warn",
			wantMsg: "level=warning msg=slow",
		},
		{
			name:    "ERR normalizes to error",
			raw:     "2026-06-07T19:31:52.000Z some ERR boom",
			wantT:   "2026-06-07T19:31:52.000Z",
			wantLvl: "error",
			wantMsg: "some ERR boom",
		},
		{
			name:    "debug recognized",
			raw:     "2026-06-07T19:31:52.000Z level=DEBUG x",
			wantT:   "2026-06-07T19:31:52.000Z",
			wantLvl: "debug",
			wantMsg: "level=DEBUG x",
		},
		{
			name:    "no level -> empty",
			raw:     "2026-06-07T19:31:52.000Z just a plain message",
			wantT:   "2026-06-07T19:31:52.000Z",
			wantLvl: "",
			wantMsg: "just a plain message",
		},
		{
			name:    "no docker ts -> empty time, level from rest",
			raw:     "level=ERROR msg=nope",
			wantT:   "",
			wantLvl: "error",
			wantMsg: "level=ERROR msg=nope",
		},
		{
			name:    "empty",
			raw:     "",
			wantT:   "",
			wantLvl: "",
			wantMsg: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseLogLine(tc.raw)
			if got.Time != tc.wantT || got.Level != tc.wantLvl || got.Msg != tc.wantMsg {
				t.Fatalf("ParseLogLine(%q) = {t:%q lvl:%q msg:%q}, want {t:%q lvl:%q msg:%q}",
					tc.raw, got.Time, got.Level, got.Msg, tc.wantT, tc.wantLvl, tc.wantMsg)
			}
		})
	}
}

// The log stream's scanner error was never checked: a line over the 1 MiB
// buffer (bufio.ErrTooLong) or a broken read ended the loop and the caller
// (the WS viewer, or AppLogs) saw a stream that had simply "ended". A silent
// stop is worse than an error surfaced in-band: the viewer keeps looking live
// while showing nothing, and an agent calling AppLogs would reason from a
// truncated tail believing it were complete.
func TestScanEndNotice(t *testing.T) {
	if _, ok := ScanEndNotice(nil); ok {
		t.Error("a clean end of stream must not produce a notice")
	}
	line, ok := ScanEndNotice(bufio.ErrTooLong)
	if !ok {
		t.Fatal("an oversized log line produced no notice")
	}
	if line.Level != "error" {
		t.Errorf("notice level = %q, want error", line.Level)
	}
	if line.Msg == "" {
		t.Error("notice has no message explaining the break")
	}
	// The notice is deliberately generic: it is handed to whoever is reading
	// the log, and the cause can name a docker socket path or a host. Callers
	// log the real error instead.
	other, ok := ScanEndNotice(errors.New("dial unix /var/run/docker.sock: connection reset"))
	if !ok {
		t.Fatal("a read failure produced no notice")
	}
	if other.Level != "error" || other.Msg == "" {
		t.Errorf("notice = %+v, want an error-level message", other)
	}
	if strings.Contains(other.Msg, "docker.sock") || strings.Contains(other.Msg, "connection reset") {
		t.Errorf("the notice leaked the underlying cause: %q", other.Msg)
	}
}
