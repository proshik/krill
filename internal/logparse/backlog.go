package logparse

import (
	"bufio"
	"container/heap"
	"io"
	"sort"
	"strings"
	"time"
)

// backlogBytes bounds the message text a backlog keeps: Swarm returns the
// tail of every task, running and stopped, and lines can be long.
const backlogBytes = 4 << 20

// Backlog reads a non-following Swarm service log stream and returns its n
// most recent non-blank lines, oldest first, and a cursor positioned after
// everything it read.
//
// Swarm applies the tail to each of the service's tasks separately (the
// running one and the stopped ones it still keeps) and interleaves their
// lines in whatever order the tasks answer, not by time. Shown as received, a
// task stopped days ago can end up below the running one, so its old lines
// read as the newest. Backlog keeps the newest n lines by docker timestamp
// (equal ones in arrival order) without holding the whole stream, and stops
// keeping older lines once their text passes backlogBytes. A line without a
// timestamp sorts with the line before it. The returned error is the
// scanner's: the lines read before it are still returned.
func Backlog(r io.Reader, n int) ([]LogLine, *Cursor, error) {
	c := &Cursor{tasks: map[string]*taskMark{}}
	h := &newest{}
	var last time.Time
	var seq, bytes int
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		l := ParseLogLine(sc.Text())
		if ts, err := time.Parse(time.RFC3339Nano, l.Time); err == nil {
			last = ts
			c.mark(l, ts)
		}
		if Blank(l) || n <= 0 {
			continue
		}
		seq++
		heap.Push(h, entry{at: last, seq: seq, line: l})
		bytes += len(l.Msg)
		for h.Len() > n || (bytes > backlogBytes && h.Len() > 1) {
			bytes -= len(heap.Pop(h).(entry).line.Msg)
		}
	}
	entries := []entry(*h)
	sort.Slice(entries, func(i, j int) bool { return entries[i].before(entries[j]) })
	out := make([]LogLine, len(entries))
	for i, e := range entries {
		out[i] = e.line
	}
	return out, c, sc.Err()
}

// Blank is a line with nothing to read (Postgres writes empty ones): shown, it
// would be a row holding only a timestamp.
func Blank(l LogLine) bool { return strings.TrimSpace(l.Msg) == "" }

type entry struct {
	at   time.Time
	seq  int
	line LogLine
}

func (a entry) before(b entry) bool {
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	return a.seq < b.seq
}

// newest is a min-heap: its root is the oldest line kept, the next to drop.
type newest []entry

func (h newest) Len() int           { return len(h) }
func (h newest) Less(i, j int) bool { return h[i].before(h[j]) }
func (h newest) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *newest) Push(x any)        { *h = append(*h, x.(entry)) }
func (h *newest) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// Cursor drops the lines of a following log stream that a backlog already
// read. The following stream is opened with a small per-task tail so that
// nothing written between the two reads is lost; that tail repeats lines of
// the backlog (and brings back old tasks' last lines), which Fresh filters
// out. Positions are kept per task: tasks run on different nodes, whose
// clocks need not agree, so one task's newest timestamp says nothing about
// another's. A line without a task (no details in the stream) counts as one
// task of its own.
type Cursor struct {
	tasks map[string]*taskMark
}

type taskMark struct {
	at     time.Time      // the task's newest timestamp in the backlog
	seen   map[string]int // its backlog lines stamped exactly at, by message
	follow bool           // the following stream has reached this task
	gap    bool           // its first following line was already new
}

func (c *Cursor) mark(l LogLine, ts time.Time) {
	m := c.tasks[l.Task]
	if m == nil {
		m = &taskMark{seen: map[string]int{}}
		c.tasks[l.Task] = m
	}
	switch {
	case ts.After(m.at):
		m.at = ts
		m.seen = map[string]int{l.Msg: 1}
	case ts.Equal(m.at):
		m.seen[l.Msg]++
	}
}

// Fresh reports whether l is newer than everything the backlog read from its
// task. A line without a timestamp, and any line of a task the backlog did
// not see (one started since), is fresh.
func (c *Cursor) Fresh(l LogLine) bool {
	m := c.tasks[l.Task]
	ts, err := time.Parse(time.RFC3339Nano, l.Time)
	if m == nil || err != nil {
		return true
	}
	fresh := true
	if ts.Before(m.at) {
		fresh = false
	} else if ts.Equal(m.at) && m.seen[l.Msg] > 0 {
		m.seen[l.Msg]--
		fresh = false
	}
	if !m.follow {
		// The tail repeats the backlog's end first; a task whose first line
		// is already new wrote more than the tail holds in between.
		m.follow = true
		m.gap = fresh
	}
	return fresh
}

// Gap reports, once, whether some task's following stream started past the
// backlog: lines written between the two reads may be missing.
func (c *Cursor) Gap() bool {
	for _, m := range c.tasks {
		if m.gap {
			for _, m := range c.tasks {
				m.gap = false
			}
			return true
		}
	}
	return false
}
