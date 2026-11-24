package logparse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// stallingReader yields its text, then blocks the way a stalled Swarm log
// read does, until it is closed.
type stallingReader struct {
	text   *strings.Reader
	closed chan struct{}
}

func stalling(text string) *stallingReader {
	return &stallingReader{text: strings.NewReader(text), closed: make(chan struct{})}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.text.Len() > 0 {
		return r.text.Read(p)
	}
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *stallingReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

// deafReader blocks forever and ignores Close: ScanWithin must not wait on it.
type deafReader struct{}

func (deafReader) Read([]byte) (int, error) { select {} }
func (deafReader) Close() error             { return nil }

func scanAll(t *testing.T, r io.ReadCloser, d time.Duration) ([]string, bool, error, time.Duration) {
	t.Helper()
	var got []string
	start := time.Now()
	cut, err := ScanWithin(r, d, func(l string) { got = append(got, l) })
	return got, cut, err, time.Since(start)
}

// The logs tab of two apps stayed blank on v0.13.2: the backlog read printed
// lines and then never ended, and the viewer sent nothing until it did.
func TestScanWithinKeepsWhatAStalledReadGave(t *testing.T) {
	r := stalling("one\ntwo\n")
	got, cut, err, took := scanAll(t, r, 100*time.Millisecond)
	if !cut || err != nil {
		t.Fatalf("cut=%v err=%v, want cut and no error", cut, err)
	}
	if strings.Join(got, "|") != "one|two" {
		t.Fatalf("lines = %q, want both lines read before the stall", got)
	}
	if took > 2*time.Second {
		t.Fatalf("took %v", took)
	}
	select {
	case <-r.closed:
	default:
		t.Fatal("the stalled reader was not closed")
	}
}

func TestScanWithinStallBeforeAnyLine(t *testing.T) {
	got, cut, err, _ := scanAll(t, stalling(""), 50*time.Millisecond)
	if !cut || err != nil || len(got) != 0 {
		t.Fatalf("lines=%q cut=%v err=%v, want nothing, cut", got, cut, err)
	}
}

func TestScanWithinReaderIgnoringClose(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, cut, _, _ := scanAll(t, deafReader{}, 50*time.Millisecond)
		if !cut {
			t.Error("not cut")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ScanWithin waited on a reader that ignores Close")
	}
}

func TestScanWithinEndsNormally(t *testing.T) {
	got, cut, err, _ := scanAll(t, io.NopCloser(strings.NewReader("a\nb")), time.Minute)
	if cut || err != nil || strings.Join(got, "|") != "a|b" {
		t.Fatalf("lines=%q cut=%v err=%v, want a|b, not cut", got, cut, err)
	}
	_, cut, err, _ = scanAll(t, io.NopCloser(strings.NewReader(strings.Repeat("x", maxLine+1))), time.Minute)
	if cut || err == nil {
		t.Fatalf("cut=%v err=%v, want the scanner's error", cut, err)
	}
}

// What a stalled backlog read gave still positions the cursor: the live
// stream's replay of those lines is dropped, a task whose live stream starts
// past them reports the gap, and a task the backlog never reached is shown
// from its live tail.
func TestBacklogWithinHandsOverToTheLiveStream(t *testing.T) {
	r := stalling(strings.Join([]string{
		"2026-11-24T10:00:01Z com.docker.swarm.task.id=a a1",
		"2026-11-24T10:00:02Z com.docker.swarm.task.id=a a2",
		"2026-11-24T09:00:01Z com.docker.swarm.task.id=b b1",
		"",
	}, "\n"))
	open := func(context.Context) (io.ReadCloser, error) { return r, nil }
	lines, c, cut, openErr, err := ReadBacklog(context.Background(), 50*time.Millisecond, 10, open)
	if !cut || err != nil || openErr != nil {
		t.Fatalf("cut=%v err=%v openErr=%v", cut, err, openErr)
	}
	if msgs(lines) != "b1|a1|a2" {
		t.Fatalf("backlog = %q", msgs(lines))
	}
	live := []string{
		"2026-11-24T10:00:01Z com.docker.swarm.task.id=a a1",
		"2026-11-24T10:00:02Z com.docker.swarm.task.id=a a2",
		"2026-11-24T10:00:03Z com.docker.swarm.task.id=a a3",
		"2026-11-24T09:00:05Z com.docker.swarm.task.id=b b5", // b1..b4: b wrote more than the overlap
		"2026-11-24T08:00:01Z com.docker.swarm.task.id=c c1", // c: never reached by the backlog
		"2026-11-24T10:00:04Z com.docker.swarm.task.id=a a4",
	}
	var shown []string
	for _, raw := range live {
		l := ParseLogLine(raw)
		if !c.Fresh(l) {
			continue
		}
		if c.Gap() {
			shown = append(shown, "GAP")
		}
		shown = append(shown, l.Msg)
	}
	if got := strings.Join(shown, "|"); got != "a3|GAP|b5|c1|a4" {
		t.Fatalf("live = %q, want no replays, the gap before b5 and c's tail", got)
	}
}

// closeRacer's Close lets the blocked Read fail first and gives the scanner
// time to finish with that error before Close returns: the error comes from
// ScanWithin's own Close and must not read as a broken stream (it made the
// viewer close with an error instead of continuing live).
type closeRacer struct{ *stallingReader }

func (r closeRacer) Close() error {
	r.stallingReader.Close()
	time.Sleep(20 * time.Millisecond)
	return nil
}

func TestScanWithinOwnCloseIsACut(t *testing.T) {
	for i := 0; i < 20; i++ {
		_, cut, err, _ := scanAll(t, closeRacer{stalling("a\n")}, 10*time.Millisecond)
		if !cut || err != nil {
			t.Fatalf("run %d: cut=%v err=%v, want a cut and no error", i, cut, err)
		}
	}
}

// A source that could not read part of the log ends with ErrIncomplete
// after the lines it has: they are kept and the read counts as cut.
func TestScanWithinIncompleteSource(t *testing.T) {
	r := io.NopCloser(io.MultiReader(strings.NewReader("a\nb\n"), errReader{fmt.Errorf("%w: 1 of 3 tasks", ErrIncomplete)}))
	got, cut, err, _ := scanAll(t, r, time.Minute)
	if !cut || err != nil || strings.Join(got, "|") != "a|b" {
		t.Fatalf("lines=%q cut=%v err=%v, want a|b, cut", got, cut, err)
	}
	r = io.NopCloser(io.MultiReader(strings.NewReader("a\n"), errReader{errors.New("broken")}))
	if _, cut, err, _ := scanAll(t, r, time.Minute); cut || err == nil {
		t.Fatalf("cut=%v err=%v: any other error is still an error", cut, err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// Opening the read counts against the same budget: an open that never
// returns (the daemon withholds its answer) is a cut with nothing read, and
// the context the source got is cancelled when ReadWithin returns, so the
// source stops whatever it still has in flight.
func TestReadWithinBoundsTheOpen(t *testing.T) {
	var got context.Context
	opened := make(chan struct{})
	start := time.Now()
	cut, openErr, scanErr := ReadWithin(context.Background(), 50*time.Millisecond, func(ctx context.Context) (io.ReadCloser, error) {
		got = ctx
		close(opened)
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(string) { t.Error("no line expected") })
	if !cut || openErr != nil || scanErr != nil {
		t.Fatalf("cut=%v openErr=%v scanErr=%v", cut, openErr, scanErr)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v", took)
	}
	<-opened
	select {
	case <-got.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the source's context was not cancelled")
	}

	// Once open returned, the scan gets the rest of the budget, not a new one.
	r := stalling("")
	start = time.Now()
	cut, _, _ = ReadWithin(context.Background(), 300*time.Millisecond, func(context.Context) (io.ReadCloser, error) {
		time.Sleep(200 * time.Millisecond)
		return r, nil
	}, func(string) {})
	if took := time.Since(start); !cut || took > 600*time.Millisecond {
		t.Fatalf("cut=%v after %v, want the whole read within the budget", cut, took)
	}

	// An open error is returned as such.
	want := errors.New("no such service")
	if _, openErr, _ := ReadWithin(context.Background(), time.Second, func(context.Context) (io.ReadCloser, error) { return nil, want }, func(string) {}); !errors.Is(openErr, want) {
		t.Fatalf("openErr = %v", openErr)
	}
}
