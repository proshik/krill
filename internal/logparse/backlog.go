package logparse

import (
	"bufio"
	"io"
	"sort"
	"time"
)

// Backlog reads a non-following Swarm service log stream and returns its n
// most recent lines, oldest first.
//
// Swarm applies the tail to each of the service's tasks separately (the
// running one and the stopped ones it still keeps) and interleaves their
// lines in whatever order the tasks answer, not by time. Shown as received, a
// task stopped days ago can end up below the running one, so its old lines
// read as the newest. Backlog merges every task's lines by their docker
// timestamp first and only then keeps the last n. A line without a timestamp
// sorts with the line before it. The returned error is the scanner's: the
// lines read before it are still returned.
func Backlog(r io.Reader, n int) ([]LogLine, error) {
	type timed struct {
		at   time.Time
		line LogLine
	}
	var entries []timed
	var last time.Time
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		l := ParseLogLine(sc.Text())
		if ts, err := time.Parse(time.RFC3339Nano, l.Time); err == nil {
			last = ts
		}
		entries = append(entries, timed{at: last, line: l})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	if n >= 0 && len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	out := make([]LogLine, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.line)
	}
	return out, sc.Err()
}

// Cursor drops the lines of a following log stream that a backlog already
// showed. The following stream is opened with a small tail so that nothing
// written between the two reads is lost; that tail repeats lines from the
// backlog (and brings back old tasks' last lines), which Fresh filters out.
type Cursor struct {
	at   time.Time      // the newest timestamp in the backlog
	seen map[string]int // backlog lines stamped exactly at, by time+message
}

// NewCursor returns a cursor positioned after the newest line of backlog.
func NewCursor(backlog []LogLine) *Cursor {
	c := &Cursor{seen: map[string]int{}}
	for _, l := range backlog {
		ts, err := time.Parse(time.RFC3339Nano, l.Time)
		if err != nil {
			continue
		}
		switch {
		case ts.After(c.at):
			c.at = ts
			c.seen = map[string]int{l.Time + " " + l.Msg: 1}
		case ts.Equal(c.at):
			c.seen[l.Time+" "+l.Msg]++
		}
	}
	return c
}

// Fresh reports whether l is newer than everything the backlog showed. A line
// without a timestamp is always fresh.
func (c *Cursor) Fresh(l LogLine) bool {
	ts, err := time.Parse(time.RFC3339Nano, l.Time)
	if err != nil || c.at.IsZero() {
		return true
	}
	if ts.Before(c.at) {
		return false
	}
	if ts.Equal(c.at) {
		k := l.Time + " " + l.Msg
		if c.seen[k] > 0 {
			c.seen[k]--
			return false
		}
	}
	return true
}
