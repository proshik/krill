package logparse

import (
	"strings"
	"testing"
)

// Swarm answers a service-logs request task by task: here the running task's
// lines come first and a task stopped days earlier comes last. The backlog
// must end with the running task's newest line.
func TestBacklogMergesTasksByTime(t *testing.T) {
	stream := strings.Join([]string{
		"2026-11-23T10:00:01Z new 1",
		"2026-11-23T10:00:02Z new 2",
		"2026-11-19T08:00:01Z old 1",
		"2026-11-23T10:00:03Z new 3",
		"2026-11-19T08:00:02Z old 2",
		"continuation without a timestamp",
	}, "\n")
	got, err := Backlog(strings.NewReader(stream), 10)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, l := range got {
		msgs = append(msgs, l.Msg)
	}
	want := "old 1|old 2|continuation without a timestamp|new 1|new 2|new 3"
	if strings.Join(msgs, "|") != want {
		t.Fatalf("order = %q, want %q", strings.Join(msgs, "|"), want)
	}

	got, _ = Backlog(strings.NewReader(stream), 2)
	if len(got) != 2 || got[0].Msg != "new 2" || got[1].Msg != "new 3" {
		t.Fatalf("window of 2 = %+v, want the two newest lines", got)
	}
}

func TestCursorDropsWhatTheBacklogShowed(t *testing.T) {
	backlog, _ := Backlog(strings.NewReader(strings.Join([]string{
		"2026-11-23T10:00:01Z a",
		"2026-11-23T10:00:02Z b",
		"2026-11-23T10:00:02Z c",
	}, "\n")), 10)
	c := NewCursor(backlog)
	cases := []struct {
		raw  string
		want bool
	}{
		{"2026-11-19T08:00:02Z old task line", false},
		{"2026-11-23T10:00:01Z a", false},
		{"2026-11-23T10:00:02Z c", false},
		{"2026-11-23T10:00:02Z c", true}, // a second, identical line is new
		{"2026-11-23T10:00:02Z d", true}, // same instant, not shown yet
		{"2026-11-23T10:00:03Z later", true},
		{"no timestamp", true},
	}
	for _, tc := range cases {
		if got := c.Fresh(ParseLogLine(tc.raw)); got != tc.want {
			t.Errorf("Fresh(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
