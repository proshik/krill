package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
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
	return taskLogsStream(ctx, logTasks(tasks), func(ctx context.Context, id string) (io.ReadCloser, error) {
		return e.cli.TaskLogs(ctx, id, opts)
	}), nil
}

// logTasks picks the tasks a non-following service log read covers, in the
// order to read them: the ones meant to run first, then the rest newest
// first, so a caller that stops early still has the current output. Like
// Swarm's own service read it skips tasks that never reached a node or never
// got as far as running: they have no output, and a task without a node
// would hold a task-scoped read open until it is scheduled.
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
		swarm.TaskStateAccepted, swarm.TaskStatePreparing, swarm.TaskStateReady, swarm.TaskStateStarting:
		return true
	}
	return false
}

// taskLogsStream reads each task's log in turn, demultiplexed, into one
// stream. A task whose log cannot be read, or not within taskLogTimeout, is
// logged and skipped: the others are still worth showing. The stream ends
// with ctx's error when ctx ends, and stops when the reader is closed.
func taskLogsStream(ctx context.Context, ids []string, open func(context.Context, string) (io.ReadCloser, error)) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		w := &lineEndWriter{w: pw}
		for _, id := range ids {
			if err := copyTaskLog(ctx, w, id, open); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	return pr
}

// copyTaskLog copies one task's log into w. It returns an error only when
// the whole read should stop: ctx ended or w's reader is gone.
func copyTaskLog(ctx context.Context, w *lineEndWriter, id string, open func(context.Context, string) (io.ReadCloser, error)) error {
	tctx, cancel := context.WithTimeout(ctx, taskLogTimeout)
	defer cancel()
	rc, err := open(tctx, id)
	if err == nil {
		_, err = stdcopy.StdCopy(w, w, rc)
		rc.Close()
	}
	if w.err != nil {
		return w.err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A task cut short may end mid-line; its next line must not run into
	// the following task's first.
	if err := w.endLine(); err != nil {
		return err
	}
	switch {
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		slog.Warn("service logs: task log read timed out, skipped the rest of it", "task", id, "timeout", taskLogTimeout)
	case err != nil:
		slog.Warn("service logs: task log unavailable", "task", id, "err", err)
	}
	return nil
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
