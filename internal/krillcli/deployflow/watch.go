// Package deployflow orchestrates one deploy: check, build, ship, trigger,
// wait. It takes every side effect as an interface, so the whole flow is
// testable with no docker and no server.
package deployflow

import (
	"strings"
	"time"
)

// Poll timing. The budget these serve is 60 requests per minute PER TOKEN,
// shared with anything else using the same credential — an MCP session in the
// user's editor, another terminal, CI. A flat two-second poll would spend 150
// requests on a five-minute deploy and start collecting 429s.
//
// The curve is tight at the start because an image deploy usually converges
// in seconds (the server pulls and rolls; there is no build), and slack after
// the first minute because anything still running then is going to take a
// while.
const (
	pollFast      = time.Second
	pollSteady    = 5 * time.Second
	pollSlow      = 10 * time.Second
	pollSlowAfter = time.Minute
)

// PollDelay returns how long to wait before poll number `attempt` (0-based),
// given how long the watch has been running.
func PollDelay(attempt int, elapsed time.Duration) time.Duration {
	switch attempt {
	case 0:
		return pollFast
	case 1:
		return 2 * pollFast
	case 2:
		return 3 * pollFast
	}
	if elapsed < pollSlowAfter {
		return pollSteady
	}
	return pollSlow
}

// LastLine returns the last non-empty line of s, which is the anchor used to
// find where a re-sent log tail continues.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// NewTail returns the part of a freshly fetched log tail that has not been
// printed yet.
//
// The server re-sends the whole last 8 KiB on every poll, so printing the
// response as it arrives would repaint the same lines once per second. anchor
// is the last line already shown; everything after its final occurrence is
// new.
//
// elided reports that the anchor was not found — the 8 KiB window moved past
// it because the app logged more than that between two polls — so the caller
// should say some lines were skipped rather than pretend the output is
// continuous.
func NewTail(anchor, cur string) (out string, elided bool) {
	cur = stripTruncationMarker(cur)
	if anchor == "" {
		return cur, false
	}
	if i := strings.LastIndex(cur, anchor); i >= 0 {
		rest := cur[i+len(anchor):]
		return strings.TrimPrefix(rest, "\n"), false
	}
	return cur, true
}

// stripTruncationMarker removes the server's "...[truncated N bytes]..."
// notice. It carries a byte count that changes on every poll, so leaving it
// in would make two otherwise identical tails compare as different and defeat
// the whole anchor search.
func stripTruncationMarker(s string) string {
	for {
		start := strings.Index(s, "...[truncated ")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "]...")
		if end < 0 {
			return s
		}
		cut := start + end + len("]...")
		s = s[:start] + strings.TrimPrefix(s[cut:], "\n")
	}
}
