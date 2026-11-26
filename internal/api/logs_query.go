package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/logparse"
)

// LogsInput is what one runtime log call asks for. Without WaitFor it reads
// a window of the log ([]LogLine); with WaitFor it waits for a line
// (LogWaitResult).
type LogsInput struct {
	Tail     int    // lines to return; 0 = default
	Level    string // minimum level
	Since    string // RFC3339, or a duration meaning that long ago
	Until    string // same forms; needs Since
	Contains string // case-insensitive substring of the message
	WaitFor  string // case-insensitive substring to wait for, in new lines only
	Timeout  int    // seconds to wait; 0 = default
}

// LogWaitResult answers a call that waited for a log line. A wait looks only
// at lines written after it started reading (FollowingSince); what came
// before is for an ordinary read with since and contains.
type LogWaitResult struct {
	Found bool `json:"found"`
	// StoppedBy says why the wait ended: match, timeout, shutdown (Krill is
	// stopping), cancelled (the caller went away), ended (the log stream
	// ended: the service is gone) or error (the live read broke).
	StoppedBy string   `json:"stopped_by"`
	Match     *LogLine `json:"match,omitempty"`
	// Lines are the lines before the match, the match, and a few after it;
	// without a match, the last lines read.
	Lines []LogLine `json:"lines"`
	// FollowingSince is when the live stream was open: lines written before
	// it are not covered by the wait. Empty when the stream never opened.
	FollowingSince string `json:"following_since,omitempty"`
	// Incomplete: the live read broke before the wait ended, so a missing
	// match is no proof the line never appeared.
	Incomplete bool `json:"incomplete,omitempty"`
}

const (
	// containsScanLines is how many of each task's last lines a contains
	// search reads when the window ends now.
	containsScanLines = 10000
	// maxWindowScanLines bounds the lines a window read scans in all: past
	// it the read stops and the answer says it is incomplete.
	maxWindowScanLines = 50000
	// Wait bounds: seconds.
	defaultLogWait = 60
	maxLogWait     = 120
	// defaultWaitContext and maxWaitContext bound the lines a wait returns
	// before the match; waitAfterLines and waitAfterGrace the lines after it.
	defaultWaitContext = 20
	maxWaitContext     = 200
	waitAfterLines     = 10
	waitAfterGrace     = time.Second
)

// parseLogTime reads a log bound: an RFC3339 timestamp, or a positive Go
// duration (10m, 2h30m) meaning that long before now.
func parseLogTime(name, v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, Invalid(fmt.Sprintf("%s %q is neither an RFC3339 time (2026-11-26T14:00:00Z) nor a duration ago (10m, 2h)", name, v))
}

// Logs is the runtime log operation both adapters call: a window of the log,
// or, with WaitFor, a wait for a line. The answer is []LogLine or
// LogWaitResult accordingly.
func (s *Service) Logs(ctx context.Context, id Identity, ref string, in LogsInput) (any, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return nil, err
	}
	if in.WaitFor != "" {
		return s.waitForLog(ctx, app, in)
	}
	if in.Timeout != 0 {
		return nil, Invalid("timeout applies to wait_for only")
	}
	return s.appLogs(ctx, app, in)
}

// logWindow is a parsed time window; zero bounds are open.
type logWindow struct{ since, until time.Time }

func (w logWindow) holds(t time.Time) bool {
	if t.IsZero() {
		return true
	}
	return (w.since.IsZero() || !t.Before(w.since)) && (w.until.IsZero() || !t.After(w.until))
}

// openLog opens a non-following read of the window, keeping tail lines of
// each task (docker.TailAll for all of them). An engine without windowed
// reads gets a plain tail read; the window is applied to the lines anyway.
func (s *Service) openLog(name string, w logWindow, tail int) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		if wl, ok := s.engine.(docker.WindowedLogs); ok {
			// Swarm's task log API takes no "until": the engine uses it only
			// to skip tasks created after it, and the window's end is applied
			// to the lines read (logWindow.holds).
			return wl.ServiceLogsWindow(ctx, name, docker.LogWindow{Since: w.since, Until: w.until, Tail: tail, NoDetails: true})
		}
		if tail == docker.TailAll {
			tail = maxWindowScanLines
		}
		return s.engine.ServiceLogs(ctx, name, false, tail)
	}
}

// stopReader ends its reader at the next Read once stop is set, so a scan can
// stop a read whose source keeps going.
type stopReader struct {
	io.ReadCloser
	stop *atomic.Bool
}

func (r stopReader) Read(p []byte) (int, error) {
	if r.stop.Load() {
		return 0, io.EOF
	}
	return r.ReadCloser.Read(p)
}

// logMessageMax bounds one returned message: a single line can reach 1 MiB,
// and an agent's context (and this process's memory, for a window held to
// sort) should not pay for that.
const logMessageMax = 16 << 10

// truncMessage cuts a message to logMessageMax bytes on a character
// boundary, saying so.
func truncMessage(m string) string {
	if len(m) <= logMessageMax {
		return m
	}
	cut := logMessageMax
	for cut > 0 && !utf8.RuneStart(m[cut]) {
		cut--
	}
	return m[:cut] + fmt.Sprintf(" …[truncated %d bytes]", len(m)-cut)
}

// windowRead is how a read of a window of a service's log ended.
type windowRead struct {
	cut     bool // incomplete: a task was skipped or capped, the read ran out of time or scan budget
	scanErr error
}

// readWindow reads the window and hands each line inside it to visit, in
// stream order (task by task), until visit returns true or the read ends:
// l carries the message cut for the answer, full the whole of it for a
// filter to match. Lines without a timestamp are windowed with the line
// before them. With capPerTask > 0 the engine returned at most that many
// lines of each task; the read is reported cut once that many were scanned,
// since some task may have had more (the stream carries no task ids — see
// docker.LogWindow.NoDetails — so this errs on the side of saying so).
func (s *Service) readWindow(ctx context.Context, name string, w logWindow, tail, capPerTask int, visit func(at time.Time, l LogLine, full string) bool) (windowRead, error) {
	var out windowRead
	var last time.Time
	var stop atomic.Bool
	scanned := 0
	open := s.openLog(name, w, tail)
	wrapped := func(ctx context.Context) (io.ReadCloser, error) {
		rc, err := open(ctx)
		if err != nil || rc == nil {
			return rc, err
		}
		return stopReader{ReadCloser: rc, stop: &stop}, nil
	}
	cut, openErr, scanErr := logparse.ReadWithin(ctx, appLogsTimeout, wrapped, func(raw string) {
		if stop.Load() {
			return
		}
		scanned++
		if scanned >= maxWindowScanLines {
			stop.Store(true)
			out.cut = true
		}
		if capPerTask > 0 && scanned == capPerTask {
			out.cut = true
		}
		parsed := logparse.ParseLogLine(raw)
		if ts, err := time.Parse(time.RFC3339Nano, parsed.Time); err == nil {
			last = ts
		}
		if !w.holds(last) {
			return
		}
		if visit(last, LogLine{Time: parsed.Time, Level: parsed.Level, Message: truncMessage(parsed.Msg)}, parsed.Msg) {
			stop.Store(true)
		}
	})
	if openErr != nil {
		return out, openErr
	}
	out.cut = out.cut || cut
	out.scanErr = scanErr
	return out, nil
}

// appLogs reads the most recent lines of a window of the app's log, filtered
// by level and substring. See AppLogs for the shape of the answer.
func (s *Service) appLogs(ctx context.Context, app db.Application, in LogsInput) ([]LogLine, error) {
	minRank, err := parseLevelFilter(in.Level)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	since, err := parseLogTime("since", in.Since, now)
	if err != nil {
		return nil, err
	}
	until, err := parseLogTime("until", in.Until, now)
	if err != nil {
		return nil, err
	}
	if !until.IsZero() && since.IsZero() {
		return nil, Invalid("until needs since: Swarm reads a log forward from a start time")
	}
	if !until.IsZero() && since.After(until) {
		return nil, Invalid("since is after until")
	}
	n := ClampTail(in.Tail)
	// No engine (unit tests, or docker unreachable): there is no log source to
	// read from, so report no lines rather than dereferencing a nil engine.
	if s.engine == nil {
		return []LogLine{}, nil
	}
	needle := strings.ToLower(in.Contains)
	// Docker's tail counts back from now, which only bounds a window that
	// ends now; a window with an end is read whole from its start.
	tail := n
	switch {
	case !until.IsZero():
		tail = docker.TailAll
	case needle != "":
		tail = containsScanLines
	}
	capPerTask := 0
	if tail == containsScanLines {
		capPerTask = containsScanLines
	}
	var entries []timedLine
	ws, err := s.readWindow(ctx, docker.ServiceName(app.ID), logWindow{since: since, until: until}, tail, capPerTask, func(at time.Time, l LogLine, full string) bool {
		if minRank >= 0 {
			if r, ok := logLevelRank(l.Level); ok && r < minRank {
				return false
			}
		}
		if needle != "" && !strings.Contains(strings.ToLower(full), needle) {
			return false
		}
		entries = append(entries, timedLine{at: at, line: l})
		// Keep memory bounded on a long window: trim to the newest n once
		// twice that many are held.
		if len(entries) >= 2*n {
			sortByLogTime(entries)
			entries = append(entries[:0:0], entries[len(entries)-n:]...)
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("app logs: service logs: %w", err)
	}
	sortByLogTime(entries)
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	lines := make([]LogLine, 0, len(entries)+1)
	for _, e := range entries {
		lines = append(lines, e.line)
	}

	// A scan error (e.g. a line over the 1 MiB limit, or a broken connection)
	// leaves everything read so far usable — append a synthetic notice line
	// describing what happened rather than either discarding the partial read
	// or returning it silently. The notice bypasses the filters above (it
	// describes the read itself, not application output to filter) and has no
	// timestamp, so it is appended after the sort: always the last line. It
	// still counts toward n, as it always has — the window gives up its oldest
	// line to make room.
	notice, ok := logparse.ScanEndNotice(ws.scanErr)
	if ok {
		// The notice handed to the reader is generic; the cause belongs here.
		slog.Error("log stream scan ended with an error", "err", ws.scanErr)
	} else if ws.cut {
		slog.Warn("app logs: log read incomplete, answering with what it read",
			"app_id", app.ID, "timeout", appLogsTimeout, "lines", len(lines))
		notice, ok = cutNotice, true
	}
	if ok {
		if len(lines) >= n {
			lines = lines[len(lines)-n+1:]
		}
		lines = append(lines, LogLine{Time: notice.Time, Level: notice.Level, Message: notice.Msg})
	}
	return lines, nil
}

// errWaitShutdown ends a wait because Krill is shutting down.
var errWaitShutdown = errors.New("krill is shutting down")

// waitContext bounds a wait by d, Krill's shutdown and ctx. stoppedBy names
// the reason it ended once it has.
func (s *Service) waitContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc, func() string) {
	parent := ctx
	ctx, cancelCause := context.WithCancelCause(ctx)
	ctx, cancelTimeout := context.WithTimeout(ctx, d)
	var stopWatch func() bool
	if s.stopping != nil {
		stopWatch = context.AfterFunc(s.stopping, func() { cancelCause(errWaitShutdown) })
	}
	cancel := func() {
		if stopWatch != nil {
			stopWatch()
		}
		cancelTimeout()
		cancelCause(nil)
	}
	stoppedBy := func() string {
		switch {
		case errors.Is(context.Cause(ctx), errWaitShutdown):
			return "shutdown"
		case parent.Err() != nil:
			return "cancelled"
		default:
			return "timeout"
		}
	}
	return ctx, cancel, stoppedBy
}

// clampWait turns a requested wait in seconds into a duration: 0 is def,
// anything above max is max, a negative value is refused.
func clampWait(name string, sec, def, max int) (time.Duration, error) {
	switch {
	case sec < 0:
		return 0, Invalid(fmt.Sprintf("%s must not be negative", name))
	case sec == 0:
		sec = def
	case sec > max:
		sec = max
	}
	return time.Duration(sec) * time.Second, nil
}

// maxLogWaits bounds the log waits running at once across every caller:
// each holds a live log stream and a bounded buffer of lines (at most
// maxWaitContext cut lines and a 64-line queue) for up to two minutes.
const maxLogWaits = 8

// waitForLog waits until a line containing in.WaitFor is written to the
// app's log after the call: it opens the live stream and reads what Docker
// sends from then on — with no time bound, so a worker whose clock lags the
// control plane's cannot hide a new line behind a timestamp cut. It does
// not read history: lines written before the stream was open are for an
// ordinary read with since and contains (the runbook closes the gap with one
// after a timeout). Lines are matched on their whole message; only the
// returned copies are cut.
func (s *Service) waitForLog(ctx context.Context, app db.Application, in LogsInput) (LogWaitResult, error) {
	if in.Since != "" || in.Contains != "" || in.Until != "" || in.Level != "" {
		return LogWaitResult{}, Invalid("wait_for waits for new lines only and cannot be combined with since, until, contains or level; search earlier lines with an ordinary read (since and contains)")
	}
	needle := strings.ToLower(in.WaitFor)
	d, err := clampWait("timeout", in.Timeout, defaultLogWait, maxLogWait)
	if err != nil {
		return LogWaitResult{}, err
	}
	nCtx := in.Tail
	switch {
	case nCtx <= 0:
		nCtx = defaultWaitContext
	case nCtx > maxWaitContext:
		nCtx = maxWaitContext
	}
	if s.engine == nil {
		return LogWaitResult{}, Unavailable("logs are unavailable: no docker engine")
	}
	select {
	case s.logWaits <- struct{}{}:
		defer func() { <-s.logWaits }()
	default:
		return LogWaitResult{}, Unavailable("too many log waits are running on this Krill; retry in a minute")
	}
	ctx, cancel, stoppedBy := s.waitContext(ctx, d)
	defer cancel()
	matches := func(full string) bool { return strings.Contains(strings.ToLower(full), needle) }

	rc, err := s.openFollow(ctx, docker.ServiceName(app.ID))
	if err != nil {
		if ctx.Err() != nil {
			return LogWaitResult{StoppedBy: stoppedBy(), Lines: []LogLine{}}, nil
		}
		return LogWaitResult{}, fmt.Errorf("wait for log: follow: %w", err)
	}
	res := LogWaitResult{FollowingSince: formatMicro(time.Now())}
	lines, followErr := followLines(ctx, rc, matches)
	before := newLineRing(nCtx)

	// The new lines, as they are written.
	var after []LogLine
	var grace <-chan time.Time
	finish := func(stopped string) (LogWaitResult, error) {
		if res.Found {
			res.Lines = append(res.Lines, after...)
			return res, nil
		}
		res.StoppedBy, res.Lines = stopped, before.lines()
		return res, nil
	}
	for {
		select {
		case fl, ok := <-lines:
			if !ok {
				if ctx.Err() != nil {
					return finish(stoppedBy())
				}
				if err := <-followErr; err != nil {
					// The live read broke (a lost connection, a line over the
					// scanner's limit): not the same as the stream ending.
					slog.Warn("wait for log: follow read failed", "app_id", app.ID, "err", err)
					res.Incomplete = true
					return finish("error")
				}
				// The log stream ended on its own: the service was removed or
				// scaled away; there is nothing more to wait for.
				return finish("ended")
			}
			if res.Found {
				after = append(after, fl.line)
				if len(after) >= waitAfterLines {
					return finish("match")
				}
				continue
			}
			if !fl.match {
				before.add(fl.line)
				continue
			}
			l := fl.line
			res.Found, res.StoppedBy, res.Match = true, "match", &l
			res.Lines = append(before.lines(), l)
			t := time.NewTimer(waitAfterGrace)
			defer t.Stop()
			grace = t.C
		case <-grace:
			return finish("match")
		case <-ctx.Done():
			// Normally the stream closes with ctx; do not depend on it.
			return finish(stoppedBy())
		}
	}
}

// openFollow opens a following read of the lines written from now on.
func (s *Service) openFollow(ctx context.Context, name string) (io.ReadCloser, error) {
	if wl, ok := s.engine.(docker.WindowedLogs); ok {
		return wl.ServiceLogsWindow(ctx, name, docker.LogWindow{Tail: 0, Follow: true, NoDetails: true})
	}
	return s.engine.ServiceLogs(ctx, name, true, 0)
}

// followed is one live line, already parsed, cut for the answer and matched
// on its whole message — so the queue between reader and waiter holds small
// lines, not raw ones of up to 1 MiB.
type followed struct {
	line  LogLine
	match bool
}

// followLines delivers rc's lines until rc ends or ctx does; then rc is
// closed and the channel closed. The error channel then carries the read's
// error (nil for a clean end), once.
func followLines(ctx context.Context, rc io.ReadCloser, matches func(full string) bool) (<-chan followed, <-chan error) {
	out := make(chan followed, 64)
	errc := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { rc.Close() })
	go func() {
		defer close(out)
		defer stop()
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			p := logparse.ParseLogLine(sc.Text())
			fl := followed{line: LogLine{Time: p.Time, Level: p.Level, Message: truncMessage(p.Msg)}, match: matches(p.Msg)}
			select {
			case out <- fl:
			case <-ctx.Done():
				errc <- nil
				return
			}
		}
		errc <- sc.Err()
	}()
	return out, errc
}

// lineRing keeps the last n lines added.
type lineRing struct {
	n   int
	buf []LogLine
}

func newLineRing(n int) *lineRing { return &lineRing{n: n} }

func (r *lineRing) add(l LogLine) {
	r.buf = append(r.buf, l)
	if len(r.buf) > 2*r.n {
		r.buf = append(r.buf[:0:0], r.buf[len(r.buf)-r.n:]...)
	}
}

func (r *lineRing) lines() []LogLine {
	if len(r.buf) > r.n {
		return append([]LogLine(nil), r.buf[len(r.buf)-r.n:]...)
	}
	return append([]LogLine{}, r.buf...)
}
