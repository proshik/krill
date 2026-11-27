package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// diagEngine is a docker.Engine with the optional capabilities the
// diagnostics read: task history, container probes and windowed logs.
type diagEngine struct {
	noopEngine
	tasks    []docker.TaskRecord
	tasksErr error
	probes   map[string]docker.ContainerProbe // by container id
	probeErr map[string]error

	mu      sync.Mutex
	windows []docker.LogWindow
	backlog string                                  // a non-following read's body
	follow  func(ctx context.Context) io.ReadCloser // a following read
}

func (e *diagEngine) ServiceTaskHistory(context.Context, string, int) ([]docker.TaskRecord, error) {
	return e.tasks, e.tasksErr
}

func (e *diagEngine) ProbeContainer(_ context.Context, _ string, containerID string) (docker.ContainerProbe, error) {
	if err := e.probeErr[containerID]; err != nil {
		return docker.ContainerProbe{}, err
	}
	return e.probes[containerID], nil
}

func (e *diagEngine) ServiceLogsWindow(ctx context.Context, _ string, w docker.LogWindow) (io.ReadCloser, error) {
	e.mu.Lock()
	e.windows = append(e.windows, w)
	e.mu.Unlock()
	if w.Follow {
		if e.follow == nil {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return e.follow(ctx), nil
	}
	return io.NopCloser(strings.NewReader(e.backlog)), nil
}

func (e *diagEngine) lastWindow() docker.LogWindow {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.windows[len(e.windows)-1]
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestWhoamiReportsInstance(t *testing.T) {
	f := newAPIFixture(t)
	f.svc.SetInstanceURL(func(context.Context) string { return "https://krill.example.test" })
	res, err := f.svc.Whoami(t.Context(), f.readIdent)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if res.Instance.URL != "https://krill.example.test" || res.Instance.Version == "" {
		t.Fatalf("instance: %+v", res.Instance)
	}
}

func TestAppStatusReportsTasksHealthAndUsage(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	// Every time here is the host's: AppStatus reads the sample window off
	// time.Now(), so the deploy's start and the sample are stamped with the
	// host's clock rather than the database's now() (the window is 1.5s wide).
	start := time.Now().Truncate(time.Microsecond)
	if _, err := f.pool.Exec(t.Context(), "UPDATE deployments SET started_at = $2 WHERE id = $1", depID, start); err != nil {
		t.Fatal(err)
	}
	// Task times are offsets from the deploy's start, in milliseconds so the
	// sample inserted below is newer than every one of them.
	at := func(d time.Duration) time.Time { return start.Add(d / time.Minute * time.Millisecond) }
	eng := &diagEngine{
		tasks: []docker.TaskRecord{
			// newest first, as the engine returns them
			{ID: "aaaaaaaaaaaaaaaa1", NodeID: "n1", NodeName: "node-1", State: "running", Desired: "running", Created: at(3 * time.Minute), Updated: at(3 * time.Minute), ContainerID: "c-run"},
			{ID: "bbbbbbbbbbbbbbbb2", NodeID: "n1", NodeName: "node-1", State: "failed", Desired: "shutdown", Err: "task: non-zero exit (1)", ExitCode: 1, Created: at(2 * time.Minute), Updated: at(150 * time.Second)},
			{ID: "cccccccccccccccc3", NodeID: "n2", NodeName: "node-2", State: "starting", Desired: "running", Created: at(time.Minute), Updated: at(time.Minute), ContainerID: "c-start"},
			{ID: "dddddddddddddddd4", NodeID: "n1", NodeName: "node-1", State: "complete", Desired: "shutdown", Created: at(30 * time.Second), Updated: at(time.Minute)},
			// before the deploy: not counted
			{ID: "eeeeeeeeeeeeeeee5", NodeID: "n1", NodeName: "node-1", State: "failed", Desired: "shutdown", Created: at(-time.Hour), Updated: at(-time.Hour)},
			{ID: "ffffffffffffffff6", NodeID: "n1", NodeName: "node-1", State: "shutdown", Desired: "shutdown", Created: at(-2 * time.Hour), Updated: at(-time.Hour)},
		},
		probes: map[string]docker.ContainerProbe{
			"c-run":   {Running: true, Health: &docker.ContainerHealth{Status: "healthy"}, MemOK: true, MemBytes: 64 << 20, MemLimitBytes: 256 << 20},
			"c-start": {Running: true, Health: &docker.ContainerHealth{Status: "starting", FailingStreak: 2, LastExitCode: 1}},
		},
	}
	svc := api.NewService(f.q, eng, nil)
	// A sample counts for a task only a full sampler interval after the task
	// (and its predecessor's last change): a row's time is its insert time.
	svc.SetMetricsInterval(500 * time.Millisecond)
	time.Sleep(1200 * time.Millisecond)
	// A sample of krill-<app> on the control plane taken after the running
	// task started (and after its predecessor on that node stopped).
	f.insertSampleAt(t, db.InsertMetricSampleParams{
		Node: "control-plane", Component: docker.ServiceName(f.appID), CpuPct: 12.5, MemBytes: 1, MemLimitBytes: 2,
	}, time.Now())

	st, err := svc.AppStatus(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Node != "node-1" {
		t.Fatalf("node %q", st.Node)
	}
	if len(st.Tasks) != 5 || st.Tasks[0].ID != "aaaaaaaaaaaa" || st.Tasks[1].Error == "" || st.Tasks[1].ExitCode != 1 {
		t.Fatalf("tasks: %+v", st.Tasks)
	}
	if st.FailedSinceDeploy == nil || *st.FailedSinceDeploy != 2 || st.FailedSince == "" {
		t.Fatalf("failed since deploy: %v (%s)", st.FailedSinceDeploy, st.FailedSince)
	}
	// The starting task's container is probed too: its health is the
	// interesting part while a healthcheck holds it back.
	if len(st.Health) != 2 || st.Health[1].Status != "starting" || st.Health[1].FailingStreak != 2 {
		t.Fatalf("health: %+v", st.Health)
	}
	if len(st.Usage) != 1 || st.Usage[0].MemBytes != 64<<20 || st.Usage[0].MemLimitBytes != 256<<20 {
		t.Fatalf("usage: %+v", st.Usage)
	}
	// The worker map is empty, so node n1 is the control plane: the only
	// task meant to run there, sampled after it started.
	if st.Usage[0].CPUPct == nil || *st.Usage[0].CPUPct != 12.5 {
		t.Fatalf("cpu not attributed: %+v", st.Usage[0])
	}
}

func TestAppStatusCPUNotAttributedToASharedNode(t *testing.T) {
	f := newAPIFixture(t)
	now := time.Now()
	eng := &diagEngine{
		tasks: []docker.TaskRecord{
			{ID: "t1", NodeID: "n1", State: "running", Desired: "running", Created: now.Add(-time.Hour), ContainerID: "c1"},
			{ID: "t2", NodeID: "n1", State: "running", Desired: "running", Created: now.Add(-time.Hour), ContainerID: "c2"},
		},
		probes: map[string]docker.ContainerProbe{
			"c1": {Running: true, MemOK: true, MemBytes: 1},
			"c2": {Running: true, MemOK: true, MemBytes: 2},
		},
	}
	// Stamped with the host's clock, like the tasks' times: the sample must be
	// inside the status's window for the refusals below to mean anything, and
	// the task created after it must stay after it whatever the database's
	// clock says.
	f.insertSampleAt(t, db.InsertMetricSampleParams{
		Node: "control-plane", Component: docker.ServiceName(f.appID), CpuPct: 50,
	}, now)
	st, err := api.NewService(f.q, eng, nil).AppStatus(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Usage) != 2 {
		t.Fatalf("usage: %+v", st.Usage)
	}
	for _, u := range st.Usage {
		if u.CPUPct != nil {
			t.Fatalf("two replicas on one node: the sample cannot be attributed, got %+v", u)
		}
	}
	// A task created after the sample was taken does not get it either.
	eng.tasks = []docker.TaskRecord{{ID: "t3", NodeID: "n1", State: "running", Desired: "running", Created: time.Now().Add(time.Minute), ContainerID: "c1"}}
	st, err = api.NewService(f.q, eng, nil).AppStatus(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Usage) != 1 || st.Usage[0].CPUPct != nil {
		t.Fatalf("a predecessor's sample must not be attributed: %+v", st.Usage)
	}
}

func TestAppStatusSurvivesUnreadableSwarmParts(t *testing.T) {
	f := newAPIFixture(t)
	eng := &diagEngine{
		tasks:    []docker.TaskRecord{{ID: "t1", NodeID: "n1", State: "running", Desired: "running", ContainerID: "c1"}},
		probeErr: map[string]error{"c1": errors.New("node unreachable")},
	}
	st, err := api.NewService(f.q, eng, nil).AppStatus(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatalf("a failed probe must not fail the status: %v", err)
	}
	if len(st.Tasks) != 1 || st.Health != nil || st.Usage != nil {
		t.Fatalf("status: %+v", st)
	}
	eng.tasksErr = errors.New("swarm busy")
	st, err = api.NewService(f.q, eng, nil).AppStatus(t.Context(), f.readIdent, f.appIDString)
	if err != nil || st.Tasks != nil || st.Replicas != "1/1" {
		t.Fatalf("a failed task read must leave the rest: %+v, %v", st, err)
	}
	if strings.Contains(mustJSON(t, st), "tasks") {
		t.Fatalf("absent parts must be omitted from the JSON: %s", mustJSON(t, st))
	}
}

const windowLogBody = "" +
	"2026-11-20T14:00:00.000Z level=info msg=boot\n" +
	"2026-11-20T14:05:00.000Z level=error msg=\"upstream TIMEOUT\"\n" +
	"2026-11-20T14:08:00.000Z level=info msg=served\n" +
	"2026-11-20T14:20:00.000Z level=error msg=\"late timeout\"\n"

func TestLogsWindowAndContains(t *testing.T) {
	f := newAPIFixture(t)
	eng := &diagEngine{backlog: windowLogBody}
	svc := api.NewService(f.q, eng, nil)

	res, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{
		Since: "2026-11-20T14:01:00Z", Until: "2026-11-20T14:10:00Z", Contains: "timeout",
	})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	lines := res.([]api.LogLine)
	if len(lines) != 1 || !strings.Contains(lines[0].Message, "upstream TIMEOUT") {
		t.Fatalf("until must be applied by Krill and contains case-insensitively: %+v", lines)
	}
	// Swarm's task logs take no until, so a window with an end is read whole
	// from its start, never cut by a tail that counts back from now; the end
	// goes to the engine only to pick the tasks.
	w := eng.lastWindow()
	if w.Tail != docker.TailAll || w.Until.IsZero() || w.Since.IsZero() || w.Follow {
		t.Fatalf("window sent to docker: %+v", w)
	}

	// Without until, docker's tail bounds the read again.
	if _, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{Since: "1h", Tail: 50}); err != nil {
		t.Fatal(err)
	}
	if w := eng.lastWindow(); w.Tail != 50 || w.Since.IsZero() {
		t.Fatalf("relative since: %+v", w)
	}

	for _, in := range []api.LogsInput{
		{Until: "10m"},
		{Since: "2026-11-26T15:00:00Z", Until: "2026-11-26T14:00:00Z"},
		{Since: "yesterday"},
		{Since: "-5m"},
		{Timeout: 5},
	} {
		_, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, in)
		var aerr *api.Error
		if !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
			t.Fatalf("%+v: want invalid, got %v", in, err)
		}
	}
}

// A wait looks only at lines written after it: a match already in the log
// is not found (an ordinary read with since and contains finds it), and no
// history is read at all.
func TestWaitForLogDoesNotReadHistory(t *testing.T) {
	f := newAPIFixture(t)
	eng := &diagEngine{backlog: windowLogBody} // the matching line is history
	svc := api.NewService(f.q, eng, nil)
	before := time.Now()
	res, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "upstream timeout", Timeout: 1})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	w := res.(api.LogWaitResult)
	if w.Found {
		t.Fatalf("a wait must not search history: %+v", w)
	}
	eng.mu.Lock()
	windows := append([]docker.LogWindow(nil), eng.windows...)
	eng.mu.Unlock()
	if len(windows) != 1 || !windows[0].Follow {
		t.Fatalf("a wait opens the live stream only, got %+v", windows)
	}
	at, err := time.Parse(time.RFC3339Nano, w.FollowingSince)
	if err != nil || at.Before(before.Add(-time.Second)) || at.After(time.Now()) {
		t.Fatalf("following_since %q must say when the live stream was open", w.FollowingSince)
	}
}

// followStream is a following log read a test writes lines into.
type followStream struct {
	pr *io.PipeReader
	pw *io.PipeWriter
}

func newFollowStream() *followStream {
	pr, pw := io.Pipe()
	return &followStream{pr: pr, pw: pw}
}

func (s *followStream) write(line string) { _, _ = io.WriteString(s.pw, line+"\n") }

func TestWaitForLogFollowsNewLines(t *testing.T) {
	f := newAPIFixture(t)
	fs := newFollowStream()
	eng := &diagEngine{follow: func(ctx context.Context) io.ReadCloser {
		context.AfterFunc(ctx, func() { fs.pw.Close() })
		return fs.pr
	}}
	svc := api.NewService(f.q, eng, nil)
	go func() {
		time.Sleep(100 * time.Millisecond)
		ts := func() string { return time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano) }
		fs.write(ts() + " level=info msg=starting")
		fs.write(ts() + " level=info msg=\"Listening on :8080\"")
		fs.write(ts() + " level=info msg=after")
	}()
	start := time.Now()
	res, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "listening on", Timeout: 10})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	w := res.(api.LogWaitResult)
	if !w.Found || w.Match == nil || !strings.Contains(w.Lines[0].Message, "starting") {
		t.Fatalf("wait: %+v", w)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a match must answer within the after-match grace, took %s", time.Since(start))
	}
	// The follow takes what is written from now on, with no time bound (a
	// worker's clock behind the control plane's cannot hide a line), and
	// without docker's log details (they can carry env values).
	eng.mu.Lock()
	follow := eng.windows[0]
	eng.mu.Unlock()
	if !follow.Follow || !follow.Since.IsZero() || follow.Tail != 0 || !follow.NoDetails {
		t.Fatalf("follow window: %+v", follow)
	}
}

func TestWaitForLogEndsOnShutdownAndTimeout(t *testing.T) {
	f := newAPIFixture(t)
	fs := newFollowStream()
	eng := &diagEngine{follow: func(ctx context.Context) io.ReadCloser {
		context.AfterFunc(ctx, func() { fs.pw.Close() })
		return fs.pr
	}}
	svc := api.NewService(f.q, eng, nil)
	stopping, stop := context.WithCancel(context.Background())
	svc.SetStopping(stopping)
	time.AfterFunc(200*time.Millisecond, stop)
	start := time.Now()
	res, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "never", Timeout: 120})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if w := res.(api.LogWaitResult); w.Found || w.StoppedBy != "shutdown" {
		t.Fatalf("wait: %+v", w)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a shutdown must end the wait at once, took %s", time.Since(start))
	}

	fs2 := newFollowStream()
	eng.follow = func(ctx context.Context) io.ReadCloser {
		context.AfterFunc(ctx, func() { fs2.pw.Close() })
		return fs2.pr
	}
	svc.SetStopping(context.Background())
	res, err = svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "never", Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w := res.(api.LogWaitResult); w.Found || w.StoppedBy != "timeout" || w.Lines == nil {
		t.Fatalf("timeout: %+v", w)
	}

	for _, in := range []api.LogsInput{
		{WaitFor: "x", Contains: "y"},
		{WaitFor: "x", Until: "1m", Since: "2m"},
		{WaitFor: "x", Level: "error"},
		{WaitFor: "x", Timeout: -1},
		{WaitFor: "x", Since: "10m"},
	} {
		_, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, in)
		var aerr *api.Error
		if !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
			t.Fatalf("%+v: want invalid, got %v", in, err)
		}
	}
}

// liveOnly serves a live stream made of body, which then ends.
func liveOnly(body string) *diagEngine {
	return &diagEngine{follow: func(context.Context) io.ReadCloser { return io.NopCloser(strings.NewReader(body)) }}
}

func TestWaitForLogMatchesBeyondTheDisplayCut(t *testing.T) {
	f := newAPIFixture(t)
	long := "2026-11-20T14:00:00.000Z " + strings.Repeat("x", 20<<10) + " READY\n"
	res, err := api.NewService(f.q, liveOnly(long), nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "ready", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if w := res.(api.LogWaitResult); !w.Found || len(w.Match.Message) > 17<<10 {
		t.Fatalf("a match past 16 KB must be found, and returned cut: found=%v", w.Found)
	}
	// The cut marker itself must not match.
	res, err = api.NewService(f.q, liveOnly(long), nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "truncated", Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w := res.(api.LogWaitResult); w.Found || w.StoppedBy != "ended" || w.Incomplete {
		t.Fatalf("the truncation marker must not match, and a clean end is not incomplete: %+v", w)
	}
	out, err := api.NewService(f.q, &diagEngine{backlog: long}, nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{Contains: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if lines := out.([]api.LogLine); len(lines) != 1 {
		t.Fatalf("contains must match the whole message: %d lines", len(lines))
	}
}

func TestLogWaitsAreBoundedAcrossCallers(t *testing.T) {
	f := newAPIFixture(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	opened := make(chan struct{}, 16)
	eng := &diagEngine{follow: func(ctx context.Context) io.ReadCloser {
		pr, pw := io.Pipe()
		go func() {
			select {
			case <-block:
			case <-ctx.Done():
			}
			pw.Close()
		}()
		opened <- struct{}{}
		return pr
	}}
	svc := api.NewService(f.q, eng, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Logs(ctx, f.readIdent, f.appIDString, api.LogsInput{WaitFor: "never", Timeout: 30})
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-opened:
		case <-time.After(10 * time.Second):
			t.Fatal("the eight waits did not start")
		}
	}
	_, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "never", Timeout: 1})
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeUnavailable {
		t.Fatalf("a ninth concurrent wait must be refused, got %v", err)
	}
	cancel()
	wg.Wait()
}

func TestWhoamiShowsTheBaseURLWithoutCredentials(t *testing.T) {
	f := newAPIFixture(t)
	f.svc.SetInstanceURL(func(context.Context) string { return "https://proxy:s3cret@krill.example.test/base?token=abc" })
	res, err := f.svc.Whoami(t.Context(), f.readIdent)
	if err != nil {
		t.Fatal(err)
	}
	if res.Instance.URL != "https://krill.example.test/base" {
		t.Fatalf("instance url %q", res.Instance.URL)
	}
}

func TestAppConfigIgnoresAnInvalidInstanceDefault(t *testing.T) {
	f := newAPIFixture(t)
	f.svc.SetResourceDefaults("lots", "0.5")
	cfg, err := f.svc.AppConfig(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.MemoryLimitSource != "none" || cfg.Resources.CPULimitSource != "default" || cfg.Resources.CPULimit != "0.5" {
		t.Fatalf("resources: %+v", cfg.Resources)
	}
}

func TestDeploymentStatusWaitNotFoundWhenTheAppIsDeleted(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	time.AfterFunc(700*time.Millisecond, func() {
		_ = f.q.DeleteApplication(context.Background(), f.appID)
	})
	_, err := f.svc.DeploymentStatusWait(t.Context(), f.readIdent, depID, 10)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("a deployment deleted during the wait: %v", err)
	}
}

func TestLogsForeignAppIsNotFound(t *testing.T) {
	f := newAPIFixture(t)
	svc := api.NewService(f.q, &diagEngine{}, nil)
	_, err := svc.Logs(t.Context(), f.otherIdent, f.appIDString, api.LogsInput{WaitFor: "x"})
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

func TestDeploymentStatusWaitReturnsWhenItEnds(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	time.AfterFunc(1500*time.Millisecond, func() {
		_ = f.q.FinishDeployment(context.Background(), db.FinishDeploymentParams{ID: depID, Status: "done", Log: "ok"})
	})
	start := time.Now()
	d, err := f.svc.DeploymentStatusWait(t.Context(), f.readIdent, depID, 30)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if d.Status != "done" || d.LogTail != "ok" || d.Settling {
		t.Fatalf("detail: %+v", d)
	}
	if el := time.Since(start); el < time.Second || el > 10*time.Second {
		t.Fatalf("waited %s", el)
	}

	// A foreign id is refused before any wait.
	start = time.Now()
	if _, err := f.svc.DeploymentStatusWait(t.Context(), f.otherIdent, depID, 60); err == nil {
		t.Fatal("foreign deployment must be not found")
	}
	if time.Since(start) > time.Second {
		t.Fatal("a refusal must not wait")
	}
	var aerr *api.Error
	if _, err := f.svc.DeploymentStatusWait(t.Context(), f.readIdent, depID, -1); !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
		t.Fatalf("negative wait: %v", err)
	}
}

func TestDeploymentStatusWaitTimesOutAndStopsOnShutdown(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	start := time.Now()
	d, err := f.svc.DeploymentStatusWait(t.Context(), f.readIdent, depID, 1)
	if err != nil || d.Status != "running" {
		t.Fatalf("timeout: %+v %v", d, err)
	}
	// The polls run to the deadline; the closing reads get a quarter second.
	if el := time.Since(start); el < 950*time.Millisecond || el > 1500*time.Millisecond {
		t.Fatalf("waited %s for a 1 s wait", el)
	}
	stopping, stop := context.WithCancel(context.Background())
	f.svc.SetStopping(stopping)
	time.AfterFunc(200*time.Millisecond, stop)
	start = time.Now()
	if d, err := f.svc.DeploymentStatusWait(t.Context(), f.readIdent, depID, 120); err != nil || d.Status != "running" {
		t.Fatalf("shutdown: %+v %v", d, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a shutdown must end the wait, took %s", time.Since(start))
	}
}

func TestAppConfigCarriesNoSecrets(t *testing.T) {
	f := newAPIFixture(t)
	ctx := t.Context()
	app, err := f.q.GetApplication(ctx, f.appID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := "wget -qO- --header=X-Probe-Key:probe-secret http://localhost/health"
	interval := "10s"
	command := "/bin/app --token=command-secret"
	if err := f.q.UpdateApplicationAdvanced(ctx, db.UpdateApplicationAdvancedParams{
		ID: app.ID, Command: &command, Replicas: 1, RestartCondition: "any",
		HealthcheckCmd: &cmd, HealthcheckInterval: &interval, PredeployCommand: "migrate --password=predeploy-secret",
	}); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	dom, err := f.q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: app.ID, Host: "hook.example.test", Tls: true, Exposed: true, Paths: "/webhook"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetDomainBasicAuth(ctx, db.SetDomainBasicAuthParams{ID: dom.ID, BasicAuthUsers: "ops:$2a$10$hash-secret"}); err != nil {
		t.Fatalf("basic auth: %v", err)
	}
	if _, err := f.q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"}); err != nil {
		t.Fatal(err)
	}
	f.svc.SetResourceDefaults("512m", "")

	cfg, err := f.svc.AppConfig(ctx, f.readIdent, "acme-proj/production/bot")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	out := mustJSON(t, cfg)
	for _, secret := range []string{"hunter2", "probe-secret", "hash-secret", "ops:", "X-Probe-Key", "command-secret", "predeploy-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("config leaks %q: %s", secret, out)
		}
	}
	if cfg.Source.Type != "image" || cfg.Source.Image != "nginx" || cfg.Source.Tag != "alpine" {
		t.Fatalf("source: %+v", cfg.Source)
	}
	if !cfg.Healthcheck.Configured || cfg.Healthcheck.Interval != "10s" {
		t.Fatalf("healthcheck: %+v", cfg.Healthcheck)
	}
	if cfg.Resources.MemoryLimit != "512m" || cfg.Resources.MemoryLimitSource != "default" || cfg.Resources.CPULimitSource != "none" {
		t.Fatalf("resources: %+v", cfg.Resources)
	}
	var hook *api.ConfigDomain
	for i := range cfg.Domains {
		if cfg.Domains[i].Host == "hook.example.test" {
			hook = &cfg.Domains[i]
		}
	}
	if hook == nil || !hook.TLS || len(hook.Paths) != 1 || len(hook.Protection) != 1 || hook.Protection[0] != "basic_auth" {
		t.Fatalf("domain: %+v", cfg.Domains)
	}
	if len(cfg.Volumes) != 1 || cfg.Volumes[0].MountPath != "/data" {
		t.Fatalf("volumes: %+v", cfg.Volumes)
	}
	if cfg.Pending.Known {
		t.Fatalf("no deployer, nothing to compare: %+v", cfg.Pending)
	}
	if _, err := f.svc.AppConfig(ctx, f.otherIdent, f.appIDString); err == nil {
		t.Fatal("foreign app must be not found")
	}
}

func TestAppConfigStripsGitCredentials(t *testing.T) {
	f := newAPIFixture(t)
	ctx := t.Context()
	bot, err := f.q.GetApplication(ctx, f.appID)
	if err != nil {
		t.Fatal(err)
	}
	app, err := f.q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: bot.EnvironmentID, Name: "web", Domain: "web.example.test", Port: 80, SourceType: "dockerfile",
		GitUrl: "https://bot:ghp_tok3n@github.com/acme/bot.git", DockerfilePath: "./build/Dockerfile", // gitleaks:allow (a fake credential the test checks is stripped)
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := f.svc.AppConfig(ctx, f.readIdent, "acme-proj/production/web")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != app.ID || cfg.Source.GitURL != "https://github.com/acme/bot.git" || cfg.Source.Branch != "main" || cfg.Source.Dockerfile != "build/Dockerfile" {
		t.Fatalf("source: %+v", cfg.Source)
	}
	if strings.Contains(mustJSON(t, cfg), "ghp_tok3n") {
		t.Fatal("git token leaked")
	}
}

func TestDisplayRepoURLShowsNoCredentials(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/bot.git":                                "https://github.com/acme/bot.git",
		"https://bot:ghp_tok3n@github.com/acme/bot.git":                  "https://github.com/acme/bot.git", // gitleaks:allow (fake)
		"https://git.example.test/acme/bot.git?private_token=tok3n#frag": "https://git.example.test/acme/bot.git",
		"https://user:pa%zz@git.example.test/acme/bot.git":               "", // unparseable: not shown at all
		"git@github.com:acme/bot.git":                                    "github.com:acme/bot.git",
		"ssh://git@github.com/acme/bot.git":                              "ssh://github.com/acme/bot.git",
		"user:tok3n@github.com:acme/bot.git":                             "", // gitleaks:allow (fake)
		"":                                                               "",
	}
	for in, want := range cases {
		if got := api.DisplayRepoURL(in); got != want {
			t.Errorf("display(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWaitForLogMatchesALineStampedBeforeTheCall(t *testing.T) {
	f := newAPIFixture(t)
	fs := newFollowStream()
	eng := &diagEngine{follow: func(ctx context.Context) io.ReadCloser {
		context.AfterFunc(ctx, func() { fs.pw.Close() })
		return fs.pr
	}}
	go func() {
		time.Sleep(100 * time.Millisecond)
		// A worker whose clock is an hour behind writes the line now.
		fs.write(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + " ready to serve")
	}()
	res, err := api.NewService(f.q, eng, nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "ready", Timeout: 10})
	if err != nil {
		t.Fatal(err)
	}
	if w := res.(api.LogWaitResult); !w.Found {
		t.Fatalf("a new line must match whatever its timestamp: %+v", w)
	}
}

// brokenFollow is a live stream that fails mid-read.
type brokenFollow struct{ done bool }

func (b *brokenFollow) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		return copy(p, "2026-11-26T10:00:00Z starting\n"), nil
	}
	return 0, errors.New("connection reset")
}
func (*brokenFollow) Close() error { return nil }

func TestWaitForLogReportsABrokenLiveRead(t *testing.T) {
	f := newAPIFixture(t)
	eng := &diagEngine{follow: func(context.Context) io.ReadCloser { return &brokenFollow{} }}
	res, err := api.NewService(f.q, eng, nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "never", Timeout: 10})
	if err != nil {
		t.Fatal(err)
	}
	w := res.(api.LogWaitResult)
	if w.Found || w.StoppedBy != "error" || !w.Incomplete || len(w.Lines) != 1 {
		t.Fatalf("a broken live read is not a clean end: %+v", w)
	}
}

func TestLogsContainsSaysWhenATaskWasCapped(t *testing.T) {
	f := newAPIFixture(t)
	var b strings.Builder
	for i := 0; i < 10000; i++ {
		b.WriteString("2026-11-20T10:00:00.000Z com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s1,com.docker.swarm.task.id=t1 busy\n")
	}
	eng := &diagEngine{backlog: b.String()}
	res, err := api.NewService(f.q, eng, nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{Contains: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	lines := res.([]api.LogLine)
	if len(lines) != 1 || !strings.Contains(lines[0].Message, "incomplete") {
		t.Fatalf("a task that filled its scan cap may hold more lines: want the incomplete notice, got %+v", lines)
	}
}

func TestLogsTruncateAnOversizedLine(t *testing.T) {
	f := newAPIFixture(t)
	eng := &diagEngine{backlog: "2026-11-20T10:00:00.000Z " + strings.Repeat("x", 100<<10) + "\n"}
	res, err := api.NewService(f.q, eng, nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lines := res.([]api.LogLine)
	if len(lines) != 1 || len(lines[0].Message) > 17<<10 || !strings.Contains(lines[0].Message, "truncated") {
		t.Fatalf("oversized line: %d bytes", len(lines[0].Message))
	}
}

// A deploy that ends after the wait's last poll, near its deadline, is
// reported ended, not running.
func TestDeploymentStatusWaitReadsOnceMoreAtTheDeadline(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	time.AfterFunc(950*time.Millisecond, func() {
		_ = f.q.FinishDeployment(context.Background(), db.FinishDeploymentParams{ID: depID, Status: "error", Log: "boom"})
	})
	d, err := f.svc.DeploymentStatusWait(t.Context(), f.readIdent, depID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "error" || d.LogTail != "boom" {
		t.Fatalf("detail at the deadline: %+v", d)
	}
}

func TestAppStatusNodeAndCPUAttributionEdges(t *testing.T) {
	f := newAPIFixture(t)
	now := time.Now()
	old := now.Add(-time.Hour)
	eng := &diagEngine{
		tasks: []docker.TaskRecord{
			// A start-first predecessor: desired shutdown, still running.
			{ID: "retiring00001", NodeID: "n1", NodeName: "node-1", State: "running", Desired: "shutdown", Created: old.Add(-time.Hour), Updated: old.Add(-time.Hour), ContainerID: "c-old"},
			// Its replacement, on a node whose name did not resolve.
			{ID: "replacement01", NodeID: "n1", State: "running", Desired: "running", Created: old, Updated: old, ContainerID: "c-new"},
		},
		probes: map[string]docker.ContainerProbe{
			"c-new": {Running: true, MemOK: true, MemBytes: 1},
		},
	}
	// The host's clock, so the sample is inside the status's window and the
	// refusals below are the attribution's, not the window's.
	f.insertSampleAt(t, db.InsertMetricSampleParams{Node: "control-plane", Component: docker.ServiceName(f.appID), CpuPct: 9}, now)
	st, err := api.NewService(f.q, eng, nil).AppStatus(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	// The node is the replacement's (meant to run), by id as its name is unknown.
	if st.Node != "n1" {
		t.Fatalf("node %q: want the running task meant to run, by id", st.Node)
	}
	if len(st.Usage) != 1 || st.Usage[0].CPUPct != nil {
		t.Fatalf("a predecessor still running on the node may own the sample: %+v", st.Usage)
	}

	// Alone on its node, but a worker is named like the control plane's key.
	eng.tasks = eng.tasks[1:]
	if _, err := f.q.CreateClusterNode(t.Context(), db.CreateClusterNodeParams{Name: "control-plane", SshHost: "192.0.2.50", SshPort: 22, SshUser: "root", SshKey: "k", HostKey: "h", SwarmNodeID: "w1"}); err != nil {
		t.Fatalf("worker: %v", err)
	}
	st, err = api.NewService(f.q, eng, nil).AppStatus(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Usage) != 1 || st.Usage[0].CPUPct != nil {
		t.Fatalf("an ambiguous sampler key must not attribute CPU: %+v", st.Usage)
	}
}

func TestAppConfigLimitThatRoundsToZeroIsNotExplicit(t *testing.T) {
	f := newAPIFixture(t)
	tiny := "0.0000000001"
	if err := f.q.UpdateApplicationAdvanced(t.Context(), db.UpdateApplicationAdvancedParams{ID: f.appID, CpuLimit: &tiny, Replicas: 1, RestartCondition: "any"}); err != nil {
		t.Fatal(err)
	}
	f.svc.SetResourceDefaults("", "0.5")
	cfg, err := f.svc.AppConfig(t.Context(), f.readIdent, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.CPULimitSource != "default" || cfg.Resources.CPULimit != "0.5" {
		t.Fatalf("a limit the deployer reads as zero falls back to the default: %+v", cfg.Resources)
	}
}

func TestLogsKeepAMessageThatStartsWithATaskField(t *testing.T) {
	f := newAPIFixture(t)
	body := "2026-11-20T10:00:00.000Z com.docker.swarm.task.id=t1,user=ada login ok\n"
	res, err := api.NewService(f.q, &diagEngine{backlog: body}, nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{Contains: "user=ada"})
	if err != nil {
		t.Fatal(err)
	}
	if lines := res.([]api.LogLine); len(lines) != 1 || !strings.HasPrefix(lines[0].Message, "com.docker.swarm.task.id=") {
		t.Fatalf("without docker details nothing but the timestamp is stripped: %+v", lines)
	}
}

func TestWaitContextKeepsExactlyTail(t *testing.T) {
	f := newAPIFixture(t)
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("2026-11-20T10:00:00.000Z line\n")
	}
	b.WriteString("2026-11-20T10:00:01.000Z READY\n")
	res, err := api.NewService(f.q, liveOnly(b.String()), nil).Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "ready", Tail: 7, Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if w := res.(api.LogWaitResult); !w.Found || len(w.Lines) != 8 {
		t.Fatalf("seven lines before the match and the match: %d lines", len(w.Lines))
	}
}
