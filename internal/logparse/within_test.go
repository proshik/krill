package logparse

import (
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
	lines, c, cut, err := BacklogWithin(r, 10, 50*time.Millisecond)
	if !cut || err != nil {
		t.Fatalf("cut=%v err=%v", cut, err)
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
