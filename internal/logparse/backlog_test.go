package logparse

import (
	"fmt"
	"strings"
	"testing"
)

func msgs(lines []LogLine) string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Msg)
	}
	return strings.Join(out, "|")
}

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
	got, _, err := Backlog(strings.NewReader(stream), 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := "old 1|old 2|continuation without a timestamp|new 1|new 2|new 3"; msgs(got) != want {
		t.Fatalf("order = %q, want %q", msgs(got), want)
	}
	got, _, _ = Backlog(strings.NewReader(stream), 2)
	if msgs(got) != "new 2|new 3" {
		t.Fatalf("window of 2 = %q, want the two newest lines", msgs(got))
	}
}

// The window is chosen while reading: a stream far longer than it keeps only
// the newest lines, equal timestamps in arrival order, and blank lines (which
// the viewer does not show) take no place in it.
func TestBacklogWindowOfALongStream(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "2026-11-23T10:%02d:%02dZ line %d\n", i/60%60, i%60, i)
		fmt.Fprintf(&b, "2026-11-23T11:00:00Z \n") // newer, but blank
	}
	b.WriteString("2026-11-23T09:00:00Z same a\n2026-11-23T09:00:00Z same b\n")
	got, _, err := Backlog(strings.NewReader(b.String()), 3)
	if err != nil {
		t.Fatal(err)
	}
	if msgs(got) != "line 3597|line 3598|line 3599" {
		t.Fatalf("window = %q, want the three newest non-blank lines", msgs(got))
	}
	got, _, _ = Backlog(strings.NewReader("2026-11-23T09:00:00Z same a\n2026-11-23T09:00:00Z same b\n"), 5)
	if msgs(got) != "same a|same b" {
		t.Fatalf("equal timestamps = %q, want arrival order", msgs(got))
	}
}

// The following stream repeats each task's tail. The cursor keeps its place
// per task: task a runs on a node whose clock is ahead, and task b's new line
// stamped before a's newest backlog line is still new.
func TestCursorPerTask(t *testing.T) {
	_, c, _ := Backlog(strings.NewReader(strings.Join([]string{
		"2026-11-23T10:00:05Z com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s,com.docker.swarm.task.id=a a newest",
		"2026-11-23T10:00:01Z com.docker.swarm.node.id=n2,com.docker.swarm.service.id=s,com.docker.swarm.task.id=b b1",
		"2026-11-23T10:00:01Z com.docker.swarm.node.id=n2,com.docker.swarm.service.id=s,com.docker.swarm.task.id=b b2",
	}, "\n")), 10)
	cases := []struct {
		raw  string
		want bool
	}{
		{"2026-11-23T10:00:05Z com.docker.swarm.task.id=a a newest", false}, // replay
		{"2026-11-23T10:00:01Z com.docker.swarm.task.id=b b2", false},       // replay
		{"2026-11-23T10:00:01Z com.docker.swarm.task.id=b b2", true},        // a second, identical line
		{"2026-11-23T10:00:03Z com.docker.swarm.task.id=b b3", true},        // new, behind a's clock
		{"2026-11-23T09:00:00Z com.docker.swarm.task.id=c c1", true},        // a task started since
		{"no timestamp", true},
	}
	for _, tc := range cases {
		if got := c.Fresh(ParseLogLine(tc.raw)); got != tc.want {
			t.Errorf("Fresh(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
	if c.Gap() {
		t.Error("every known task's following stream began with a replay: no gap")
	}
}

// A task whose following stream starts past the backlog wrote more than the
// overlap between the two reads: the cursor reports it once.
func TestCursorGap(t *testing.T) {
	_, c, _ := Backlog(strings.NewReader("2026-11-23T10:00:01Z com.docker.swarm.task.id=a a1\n"), 10)
	if !c.Fresh(ParseLogLine("2026-11-23T10:00:09Z com.docker.swarm.task.id=a a9")) {
		t.Fatal("a newer line must be fresh")
	}
	if !c.Gap() || c.Gap() {
		t.Error("the gap must be reported exactly once")
	}
}

func TestParseLogLineDetails(t *testing.T) {
	l := ParseLogLine("2026-11-23T10:00:01Z com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s1,com.docker.swarm.task.id=t1 level=info hello")
	if l.Task != "t1" || l.Msg != "level=info hello" || l.Level != "info" || l.Time != "2026-11-23T10:00:01Z" {
		t.Fatalf("parsed %+v", l)
	}
	if l := ParseLogLine("2026-11-23T10:00:01Z com.docker.swarm.whatever"); l.Task != "" || l.Msg != "com.docker.swarm.whatever" {
		t.Fatalf("a message that only looks like details must stay: %+v", l)
	}
	// A logging driver's env option adds the app's values to the details, and
	// docker sorts the keys: an uppercase variable comes before Swarm's.
	l = ParseLogLine("2026-11-23T10:00:01Z POSTGRES_PASSWORD=s3cr%20et,com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s1,com.docker.swarm.task.id=t1 ready")
	if l.Task != "t1" || l.Msg != "ready" || strings.Contains(l.Msg, "s3cr") {
		t.Fatalf("the whole attribute block must be stripped: %+v", l)
	}
}

// Once a task's following stream has passed its replay, every line is new:
// a worker clock that steps back must not hide the task's output.
func TestCursorAfterReplayEverythingIsNew(t *testing.T) {
	_, c, _ := Backlog(strings.NewReader("2026-11-23T10:00:05Z com.docker.swarm.task.id=a a5\n"), 10)
	if c.Fresh(ParseLogLine("2026-11-23T10:00:05Z com.docker.swarm.task.id=a a5")) {
		t.Fatal("the replayed line must be dropped")
	}
	if !c.Fresh(ParseLogLine("2026-11-23T10:00:06Z com.docker.swarm.task.id=a a6")) {
		t.Fatal("a newer line must be fresh")
	}
	if !c.Fresh(ParseLogLine("2026-11-23T10:00:01Z com.docker.swarm.task.id=a after the clock stepped back")) {
		t.Error("after the replay, an earlier timestamp is still a new line")
	}
	if c.Gap() {
		t.Error("the task's stream began with its replay: no gap")
	}
}

// Blank lines are part of a task's replay: a tail of replayed blanks and one
// new line is not a gap.
func TestCursorBlankReplayIsNotAGap(t *testing.T) {
	var b strings.Builder
	b.WriteString("2026-11-23T10:00:00Z com.docker.swarm.task.id=a visible\n")
	for i := 0; i < 50; i++ {
		b.WriteString("2026-11-23T10:00:01Z com.docker.swarm.task.id=a \n")
	}
	_, c, _ := Backlog(strings.NewReader(b.String()), 10)
	for i := 0; i < 49; i++ {
		if c.Fresh(ParseLogLine("2026-11-23T10:00:01Z com.docker.swarm.task.id=a ")) {
			t.Fatalf("replayed blank %d taken as new", i)
		}
	}
	if !c.Fresh(ParseLogLine("2026-11-23T10:00:02Z com.docker.swarm.task.id=a new")) {
		t.Fatal("the new line must be fresh")
	}
	if c.Gap() {
		t.Error("the overlap began with replayed blanks: no gap")
	}
}
