package logparse

import (
	"bufio"
	"container/heap"
	"hash/fnv"
	"io"
	"sort"
	"strings"
	"time"
)

// BacklogTimeout bounds how long a caller waits for a non-following service
// log read (BacklogWithin, ScanWithin) before it uses what it has.
const BacklogTimeout = 10 * time.Second

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
	b := newBacklog(n)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		b.add(sc.Text())
	}
	lines, c := b.result()
	return lines, c, sc.Err()
}

// BacklogWithin is Backlog that gives up after d: Swarm's non-following log
// read can stall without ending, and a viewer waiting on it would see
// nothing at all. cut reports that d ran out first; the lines read by then
// are returned and the cursor covers exactly them, so a following stream
// started next still drops their replays and reports a gap for a task it
// finds already past them (see ScanWithin for what happens to r).
func BacklogWithin(r io.ReadCloser, n int, d time.Duration) (lines []LogLine, c *Cursor, cut bool, err error) {
	b := newBacklog(n)
	cut, err = ScanWithin(r, d, b.add)
	lines, c = b.result()
	return lines, c, cut, err
}

// backlog collects the newest lines of a stream, see Backlog.
type backlog struct {
	n     int
	c     *Cursor
	h     *newest
	last  time.Time
	seq   int
	bytes int
}

func newBacklog(n int) *backlog {
	return &backlog{n: n, c: &Cursor{tasks: map[string]*taskMark{}}, h: &newest{}}
}

func (b *backlog) add(raw string) {
	l := ParseLogLine(raw)
	if ts, err := time.Parse(time.RFC3339Nano, l.Time); err == nil {
		b.last = ts
		b.c.mark(l, ts)
	}
	if Blank(l) || b.n <= 0 {
		return
	}
	b.seq++
	heap.Push(b.h, entry{at: b.last, seq: b.seq, line: l})
	b.bytes += len(l.Msg)
	for b.h.Len() > b.n || (b.bytes > backlogBytes && b.h.Len() > 1) {
		b.bytes -= len(heap.Pop(b.h).(entry).line.Msg)
	}
}

func (b *backlog) result() ([]LogLine, *Cursor) {
	entries := []entry(*b.h)
	sort.Slice(entries, func(i, j int) bool { return entries[i].before(entries[j]) })
	out := make([]LogLine, len(entries))
	for i, e := range entries {
		out[i] = e.line
	}
	return out, b.c
}

// maxLine is the longest log line a scan accepts (bufio.ErrTooLong past it).
const maxLine = 1024 * 1024

// ScanWithin calls line for each line of r, in order, until r ends or d has
// passed. A read still running at d is abandoned: r is closed (which ends a
// docker log read) and ScanWithin returns with cut set, after handing line
// whatever was already read, without waiting for the read to notice — a
// reader that ignores Close cannot hold the caller either. err is the
// scanner's error when r ended first.
func ScanWithin(r io.ReadCloser, d time.Duration, line func(string)) (cut bool, err error) {
	lines := make(chan string, 64)
	scanErr := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), maxLine)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-stop:
				return
			}
		}
		scanErr <- sc.Err()
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				select {
				case err = <-scanErr:
				default: // stopped, not ended: cannot happen before stop
				}
				return false, err
			}
			line(l)
		case <-timer.C:
			close(stop)
			r.Close()
			for {
				select {
				case l, ok := <-lines:
					if !ok {
						select {
						case err = <-scanErr: // r ended right at d after all
							return false, err
						default:
							return true, nil
						}
					}
					line(l)
				default:
					return true, nil
				}
			}
		}
	}
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
// task of its own. Docker sends a task's tail before its live lines, in order,
// so once a task's line is new, every later one is: from then on its
// timestamps are not compared at all (a worker's clock may step back).
type Cursor struct {
	tasks map[string]*taskMark
}

type taskMark struct {
	at     time.Time         // the task's newest timestamp in the backlog
	seen   map[uint64]uint32 // its backlog lines stamped exactly at, by message fingerprint
	done   bool              // its replay is over: every following line is new
	follow bool              // the following stream has reached this task
	gap    bool              // its first following line was already new
}

// fingerprint keys the replay check without keeping the message itself.
func fingerprint(msg string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(msg))
	return h.Sum64()
}

func (c *Cursor) mark(l LogLine, ts time.Time) {
	m := c.tasks[l.Task]
	if m == nil {
		m = &taskMark{seen: map[uint64]uint32{}}
		c.tasks[l.Task] = m
	}
	switch {
	case ts.After(m.at):
		m.at = ts
		clear(m.seen)
		m.seen[fingerprint(l.Msg)] = 1
	case ts.Equal(m.at):
		m.seen[fingerprint(l.Msg)]++
	}
}

// Fresh reports whether l is newer than everything the backlog read from its
// task. A line without a timestamp, and any line of a task the backlog did
// not see (one started since), is fresh. Such a task has no replay to tell a
// gap by: if it started between the two reads and wrote more than the tail
// holds, its first lines are missing without a notice (docker's log API
// offers no continuity marker to detect it). Feed it every line of the following
// stream, blank ones included: they are part of the replay too.
func (c *Cursor) Fresh(l LogLine) bool {
	m := c.tasks[l.Task]
	if m == nil || m.done {
		return true
	}
	ts, err := time.Parse(time.RFC3339Nano, l.Time)
	if err != nil {
		return true
	}
	fresh := true
	if ts.Before(m.at) {
		fresh = false
	} else if ts.Equal(m.at) {
		if k := fingerprint(l.Msg); m.seen[k] > 0 {
			m.seen[k]--
			fresh = false
		}
	}
	if !m.follow {
		// The tail repeats the backlog's end first; a task whose first line
		// is already new wrote more than the tail holds in between.
		m.follow = true
		m.gap = fresh
	}
	if fresh {
		m.done, m.seen = true, nil
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
