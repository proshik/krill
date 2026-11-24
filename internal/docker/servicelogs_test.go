package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
)

func TestLogTasksOrderAndSkip(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 24, 10, m, 0, 0, time.UTC) }
	task := func(id, node string, desired, state swarm.TaskState, m int) swarm.Task {
		return swarm.Task{ID: id, NodeID: node, DesiredState: desired, Status: swarm.TaskStatus{State: state, Timestamp: at(m)}}
	}
	got := logTasks([]swarm.Task{
		task("old", "n1", swarm.TaskStateShutdown, swarm.TaskStateShutdown, 1),
		task("newer", "n1", swarm.TaskStateShutdown, swarm.TaskStateFailed, 5),
		task("pending", "", swarm.TaskStateRunning, swarm.TaskStatePending, 9),
		task("starting", "n2", swarm.TaskStateRunning, swarm.TaskStateStarting, 9),
		task("run-a", "n1", swarm.TaskStateRunning, swarm.TaskStateRunning, 3),
		task("run-b", "n2", swarm.TaskStateRunning, swarm.TaskStateRunning, 4),
		task("never-ran", "n1", swarm.TaskStateShutdown, swarm.TaskStateShutdown, 2),
	})
	if want := "run-b|run-a|newer|never-ran|old"; strings.Join(got, "|") != want {
		t.Fatalf("tasks = %q, want %q", strings.Join(got, "|"), want)
	}
}

// muxed is a task log as the daemon sends it: stdcopy frames.
func muxed(lines ...string) []byte {
	var b bytes.Buffer
	w := stdcopy.NewStdWriter(&b, stdcopy.Stdout)
	for _, l := range lines {
		w.Write([]byte(l))
	}
	return b.Bytes()
}

// stalledBody yields its content, then blocks until ctx ends (an HTTP body
// whose request context ended fails the same way).
type stalledBody struct {
	ctx context.Context
	r   *bytes.Reader
}

func (s *stalledBody) Read(p []byte) (int, error) {
	if s.r.Len() > 0 {
		return s.r.Read(p)
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
func (s *stalledBody) Close() error { return nil }

// A whole-service read could stall for good (several tasks on one stream);
// the tasks are read one by one, and one that stalls or fails costs its own
// part only.
func TestTaskLogsStreamReadsEachTaskInTurn(t *testing.T) {
	old := taskLogTimeout
	taskLogTimeout = 100 * time.Millisecond
	t.Cleanup(func() { taskLogTimeout = old })

	open := func(ctx context.Context, id string) (io.ReadCloser, error) {
		switch id {
		case "a":
			return io.NopCloser(bytes.NewReader(muxed("a1\n", "a2\n"))), nil
		case "stall":
			return &stalledBody{ctx: ctx, r: bytes.NewReader(muxed("s1\n", "s2 cut mid-line"))}, nil
		case "gone":
			return nil, errors.New("no such task")
		}
		return io.NopCloser(bytes.NewReader(muxed("b1\n"))), nil
	}
	start := time.Now()
	rc := taskLogsStream(context.Background(), []string{"a", "stall", "gone", "b"}, open)
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "a1\na2\ns1\ns2 cut mid-line\nb1\n"; string(out) != want {
		t.Fatalf("stream = %q, want %q", out, want)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v: the stalled task was waited for", took)
	}
}

func TestTaskLogsStreamStopsWithItsContextAndReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opened := make(chan string, 10)
	open := func(ctx context.Context, id string) (io.ReadCloser, error) {
		opened <- id
		return &stalledBody{ctx: ctx, r: bytes.NewReader(muxed(id + "\n"))}, nil
	}
	rc := taskLogsStream(ctx, []string{"a", "b"}, open)
	buf := make([]byte, 2)
	if _, err := io.ReadFull(rc, buf); err != nil || string(buf) != "a\n" {
		t.Fatalf("first line %q, %v", buf, err)
	}
	cancel()
	if _, err := io.ReadAll(rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d tasks after the context ended, want 1", len(opened))
	}

	// A reader that goes away stops the copy too.
	done := make(chan struct{})
	open2 := func(ctx context.Context, id string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(muxed(strings.Repeat("x", 100) + "\n"))), nil
	}
	rc = taskLogsStream(context.Background(), []string{"a", "b", "c"}, func(ctx context.Context, id string) (io.ReadCloser, error) {
		if id == "c" {
			close(done)
		}
		return open2(ctx, id)
	})
	rc.Close()
	select {
	case <-done:
		t.Fatal("kept reading tasks after the reader was closed")
	case <-time.After(200 * time.Millisecond):
	}
}
