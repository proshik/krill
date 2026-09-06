package deployflow_test

import (
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/deployflow"
)

// TestPollScheduleStaysInsideTheRateBudget is the executable form of the
// argument for the curve. The API allows 60 requests per minute per token and
// that budget is SHARED with everything else using the same credential, so a
// watch that polls faster does not just waste requests, it starts failing the
// user's other calls. Anyone tempted to "poll a bit more often" has to make
// this test agree first.
func TestPollScheduleStaysInsideTheRateBudget(t *testing.T) {
	count := func(window time.Duration) int {
		var elapsed time.Duration
		n := 0
		for attempt := 0; ; attempt++ {
			elapsed += deployflow.PollDelay(attempt, elapsed)
			if elapsed > window {
				return n
			}
			n++
		}
	}

	if got := count(time.Minute); got > 25 {
		t.Fatalf("%d polls in the first minute; the per-token budget is 60/min and is shared", got)
	}
	if got := count(10 * time.Minute); got > 90 {
		t.Fatalf("%d polls in ten minutes, too many for a shared budget", got)
	}
	// It must still be responsive: a deploy that converges in five seconds
	// should not be reported ten seconds late.
	if got := count(5 * time.Second); got < 2 {
		t.Fatalf("only %d polls in the first five seconds; a fast deploy would look slow", got)
	}
}

func TestPollDelayWidens(t *testing.T) {
	tests := []struct {
		attempt int
		elapsed time.Duration
		want    time.Duration
	}{
		{0, 0, time.Second},
		{1, time.Second, 2 * time.Second},
		{2, 3 * time.Second, 3 * time.Second},
		{3, 6 * time.Second, 5 * time.Second},
		{9, 30 * time.Second, 5 * time.Second},
		{20, 61 * time.Second, 10 * time.Second},
		{99, 9 * time.Minute, 10 * time.Second},
	}
	for _, tc := range tests {
		if got := deployflow.PollDelay(tc.attempt, tc.elapsed); got != tc.want {
			t.Fatalf("PollDelay(%d, %v) = %v, want %v", tc.attempt, tc.elapsed, got, tc.want)
		}
	}
}

func TestNewTail(t *testing.T) {
	tests := []struct {
		name       string
		anchor     string
		cur        string
		wantOut    string
		wantElided bool
	}{
		{
			name:    "first poll prints everything",
			anchor:  "",
			cur:     "one\ntwo\n",
			wantOut: "one\ntwo\n",
		},
		{
			name:    "unchanged tail prints nothing",
			anchor:  "two",
			cur:     "one\ntwo\n",
			wantOut: "",
		},
		{
			name:    "only the appended lines",
			anchor:  "two",
			cur:     "one\ntwo\nthree\nfour\n",
			wantOut: "three\nfour\n",
		},
		{
			// The 8 KiB window moved past everything we had printed.
			name:       "window slid past the anchor",
			anchor:     "long gone line",
			cur:        "much\nlater\noutput\n",
			wantOut:    "much\nlater\noutput\n",
			wantElided: true,
		},
		{
			// The marker's byte count changes on every poll, so it has to go
			// before anything is compared.
			name:    "truncation marker is stripped",
			anchor:  "two",
			cur:     "head\n...[truncated 4211 bytes]...\none\ntwo\nthree\n",
			wantOut: "three\n",
		},
		{
			name:    "repeated line anchors on the last occurrence",
			anchor:  "same",
			cur:     "same\nother\nsame\nafter\n",
			wantOut: "after\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, elided := deployflow.NewTail(tc.anchor, tc.cur)
			if out != tc.wantOut {
				t.Fatalf("NewTail out = %q, want %q", out, tc.wantOut)
			}
			if elided != tc.wantElided {
				t.Fatalf("NewTail elided = %v, want %v", elided, tc.wantElided)
			}
		})
	}
}

// TestNewTailNeverRepaintsAcrossPolls walks a growing log the way a real
// watch does and asserts that concatenating the pieces reproduces the log
// exactly once — no duplicated lines, none lost.
func TestNewTailNeverRepaintsAcrossPolls(t *testing.T) {
	polls := []string{
		"pulling image\n",
		"pulling image\nstarting\n",
		"pulling image\nstarting\nhealthy\n",
		"pulling image\nstarting\nhealthy\n",
		"pulling image\nstarting\nhealthy\ndone\n",
	}
	var printed strings.Builder
	anchor := ""
	for _, p := range polls {
		out, elided := deployflow.NewTail(anchor, p)
		if elided {
			t.Fatalf("no window should have slid in this sequence")
		}
		printed.WriteString(out)
		anchor = deployflow.LastLine(p)
	}
	got := printed.String()
	want := "pulling image\nstarting\nhealthy\ndone\n"
	if got != want {
		t.Fatalf("reassembled log = %q, want %q", got, want)
	}
}

func TestLastLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a\nb\nc\n", "c"},
		{"a\nb\nc", "c"},
		{"only\n", "only"},
		{"trailing\n\n\n", "trailing"},
		{"", ""},
		{"\n\n", ""},
	}
	for _, tc := range tests {
		if got := deployflow.LastLine(tc.in); got != tc.want {
			t.Fatalf("LastLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
