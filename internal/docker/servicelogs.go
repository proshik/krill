package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/proshik/krill/internal/logparse"
)

// taskLogTimeout bounds one task's part of a non-following service log read.
// A task's tail normally arrives in tens of milliseconds; one that takes
// longer (a node that stopped answering) is skipped, not waited for.
var taskLogTimeout = 5 * time.Second

func (e *dockerEngine) ServiceLogs(ctx context.Context, name string, follow bool, tail int) (io.ReadCloser, error) {
	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       strconv.Itoa(tail),
		Timestamps: true,
		// Each line carries its task: the log viewer keeps its place per task
		// (logparse.Cursor), and logparse.ParseLogLine strips the attributes.
		Details: true,
	}
	if follow {
		rc, err := e.cli.ServiceLogs(ctx, name, opts)
		if err != nil {
			return nil, err
		}
		return NewLogReader(rc), nil
	}
	// A non-following read of a whole service can stall for good: the Swarm
	// worker answers it with one goroutine per task, all sending on the same
	// gRPC stream, and once the tails are large enough (200 lines over five
	// tasks was enough on Docker 28 and 29) the stream stops before its end —
	// sometimes with a task's lines missing, sometimes after all of them, and
	// the read never returns. Reading the tasks one at a time, each through a
	// subscription of its own, never stalled.
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", name))})
	if err != nil {
		return nil, err
	}
	// Closing the reader cancels the task reads behind it: one still waiting
	// on a silent task would otherwise only notice at its next write.
	ctx, cancel := context.WithCancel(ctx)
	rc := taskLogsStream(ctx, logTasks(tasks), func(ctx context.Context, id string) (io.ReadCloser, error) {
		return e.cli.TaskLogs(ctx, id, opts)
	})
	return cancelOnClose{ReadCloser: rc, cancel: cancel}, nil
}

// cancelOnClose cancels a context when its reader is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}

// logTasks picks the tasks a non-following service log read covers, in the
// order to read them: the ones meant to run first, then the rest newest
// first, so a caller that stops early still has the current output. It
// skips tasks that never reached a node (a task-scoped read of one would
// stay open until it is scheduled) and tasks whose container has not been
// started yet. A starting task is kept: during a start-first update its
// container runs (and logs) while its healthcheck is pending, and Swarm's
// own service read showed those lines whenever the node also held another
// task of the service.
func logTasks(tasks []swarm.Task) []string {
	var picked []swarm.Task
	for _, t := range tasks {
		if t.NodeID == "" || notStarted(t.Status.State) {
			continue
		}
		picked = append(picked, t)
	}
	sort.SliceStable(picked, func(i, j int) bool {
		ri := picked[i].DesiredState == swarm.TaskStateRunning
		rj := picked[j].DesiredState == swarm.TaskStateRunning
		if ri != rj {
			return ri
		}
		return picked[i].Status.Timestamp.After(picked[j].Status.Timestamp)
	})
	ids := make([]string, len(picked))
	for i, t := range picked {
		ids[i] = t.ID
	}
	return ids
}

func notStarted(s swarm.TaskState) bool {
	switch s {
	case swarm.TaskStateNew, swarm.TaskStateAllocated, swarm.TaskStatePending, swarm.TaskStateAssigned,
		swarm.TaskStateAccepted, swarm.TaskStatePreparing, swarm.TaskStateReady:
		return true
	}
	return false
}

// taskLogsStream reads each task's log in turn, demultiplexed, into one
// stream. A task whose log cannot be read, or not within taskLogTimeout, is
// logged and skipped, and the stream then ends with logparse.ErrIncomplete
// after the other tasks' lines, so a caller can say the history is partial.
// The stream ends with ctx's error when ctx ends, and stops when the reader
// is closed (cancel ctx too to stop a task read that has not written yet).
func taskLogsStream(ctx context.Context, ids []string, open func(context.Context, string) (io.ReadCloser, error)) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		w := &lineEndWriter{w: pw}
		failed := 0
		for _, id := range ids {
			ok, err := copyTaskLog(ctx, w, id, open)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if !ok {
				failed++
			}
		}
		if failed > 0 {
			pw.CloseWithError(fmt.Errorf("%w: %d of %d tasks", logparse.ErrIncomplete, failed, len(ids)))
			return
		}
		pw.Close()
	}()
	return pr
}

// copyTaskLog copies one task's log into w and reports whether all of it
// was read. It returns an error only when the whole read should stop: ctx
// ended or w's reader is gone.
func copyTaskLog(ctx context.Context, w *lineEndWriter, id string, open func(context.Context, string) (io.ReadCloser, error)) (bool, error) {
	tctx, cancel := context.WithTimeout(ctx, taskLogTimeout)
	defer cancel()
	rc, err := open(tctx, id)
	if err == nil {
		_, err = stdcopy.StdCopy(w, w, rc)
		rc.Close()
	}
	if w.err != nil {
		return false, w.err
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// A task cut short may end mid-line; its next line must not run into
	// the following task's first.
	if err := w.endLine(); err != nil {
		return false, err
	}
	switch {
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		slog.Warn("service logs: task log read timed out, skipped the rest of it", "task", id, "timeout", taskLogTimeout)
		return false, nil
	case err != nil:
		slog.Warn("service logs: task log unavailable", "task", id, "err", err)
		return false, nil
	}
	return true, nil
}

// lineEndWriter remembers whether the last byte written ended a line, and
// the first write error (the reader went away).
type lineEndWriter struct {
	w       io.Writer
	err     error
	partial bool
}

func (l *lineEndWriter) Write(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	n, err := l.w.Write(p)
	if n > 0 {
		l.partial = p[n-1] != '\n'
	}
	if err != nil {
		l.err = err
	}
	return n, err
}

func (l *lineEndWriter) endLine() error {
	if !l.partial {
		return nil
	}
	_, err := l.Write([]byte{'\n'})
	return err
}
