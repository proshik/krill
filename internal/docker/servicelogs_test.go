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

	"github.com/proshik/krill/internal/logparse"
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
	if want := "starting|run-b|run-a|newer|never-ran|old"; strings.Join(got, "|") != want {
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
	if !errors.Is(err, logparse.ErrIncomplete) {
		t.Fatalf("read ended with %v, want ErrIncomplete: two tasks were not read in full", err)
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

func TestTaskLogsStreamEndsCleanWhenEveryTaskWasRead(t *testing.T) {
	rc := taskLogsStream(context.Background(), []string{"a", "b"}, func(ctx context.Context, id string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(muxed(id + "\n"))), nil
	})
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil || string(out) != "a\nb\n" {
		t.Fatalf("stream = %q, %v", out, err)
	}
}

// Each kind of failed task read on its own marks the stream incomplete: a
// body that stalls past taskLogTimeout, and an open that fails.
func TestTaskLogsStreamEachFailureIsIncomplete(t *testing.T) {
	old := taskLogTimeout
	taskLogTimeout = 50 * time.Millisecond
	t.Cleanup(func() { taskLogTimeout = old })
	for name, failing := range map[string]func(ctx context.Context) (io.ReadCloser, error){
		"timeout": func(ctx context.Context) (io.ReadCloser, error) {
			return &stalledBody{ctx: ctx, r: bytes.NewReader(muxed("s1\n"))}, nil
		},
		"open": func(context.Context) (io.ReadCloser, error) { return nil, errors.New("no such task") },
	} {
		rc := taskLogsStream(context.Background(), []string{"a", "bad"}, func(ctx context.Context, id string) (io.ReadCloser, error) {
			if id == "bad" {
				return failing(ctx)
			}
			return io.NopCloser(bytes.NewReader(muxed("a1\n"))), nil
		})
		out, err := io.ReadAll(rc)
		rc.Close()
		if !errors.Is(err, logparse.ErrIncomplete) || !strings.HasPrefix(string(out), "a1\n") {
			t.Fatalf("%s: stream %q ended with %v, want a1 then ErrIncomplete", name, out, err)
		}
	}
}

// Closing the engine's reader cancels the task read still waiting behind
// it, without a write to notice the close by.
func TestCancelOnCloseStopsTheTaskReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan context.Context, 1)
	rc := cancelOnClose{ReadCloser: taskLogsStream(ctx, []string{"silent", "next"}, func(ctx context.Context, id string) (io.ReadCloser, error) {
		if id == "next" {
			t.Error("opened the next task after the reader was closed")
		}
		started <- ctx
		return &stalledBody{ctx: ctx, r: bytes.NewReader(nil)}, nil
	}), cancel: cancel}
	tctx := <-started
	rc.Close()
	select {
	case <-tctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the task read was not cancelled")
	}
	time.Sleep(50 * time.Millisecond)
}
