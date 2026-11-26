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
	dep, err := f.q.GetDeployment(t.Context(), depID)
	if err != nil {
		t.Fatal(err)
	}
	// Task times are offsets from the deploy's start, in milliseconds so the
	// sample inserted below is newer than every one of them.
	start := dep.StartedAt
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
	time.Sleep(20 * time.Millisecond)
	// A sample of krill-<app> on the control plane taken after the running
	// task started (and after its predecessor on that node stopped).
	if err := f.q.InsertMetricSample(t.Context(), db.InsertMetricSampleParams{
		Node: "control-plane", Component: docker.ServiceName(f.appID), CpuPct: 12.5, MemBytes: 1, MemLimitBytes: 2,
	}); err != nil {
		t.Fatalf("sample: %v", err)
	}

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
	if err := f.q.InsertMetricSample(t.Context(), db.InsertMetricSampleParams{
		Node: "control-plane", Component: docker.ServiceName(f.appID), CpuPct: 50,
	}); err != nil {
		t.Fatal(err)
	}
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
	// from its start, never cut by a tail that counts back from now.
	w := eng.lastWindow()
	if w.Tail != docker.TailAll || !w.Until.IsZero() || w.Since.IsZero() || w.Follow {
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

func TestWaitForLogFindsALineInHistory(t *testing.T) {
	f := newAPIFixture(t)
	eng := &diagEngine{backlog: windowLogBody}
	svc := api.NewService(f.q, eng, nil)
	res, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{
		WaitFor: "upstream timeout", Since: "2026-11-20T13:00:00Z", Tail: 1,
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	w := res.(api.LogWaitResult)
	if !w.Found || w.StoppedBy != "match" || w.Match == nil || !strings.Contains(w.Match.Message, "upstream") {
		t.Fatalf("wait: %+v", w)
	}
	// One line before (tail=1), the match, then the lines after it.
	if len(w.Lines) != 4 || !strings.Contains(w.Lines[0].Message, "boot") || !strings.Contains(w.Lines[2].Message, "served") {
		t.Fatalf("lines: %+v", w.Lines)
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
	if w := eng.lastWindow(); !w.Follow || w.Since.IsZero() {
		t.Fatalf("follow window: %+v", w)
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
	} {
		_, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, in)
		var aerr *api.Error
		if !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
			t.Fatalf("%+v: want invalid, got %v", in, err)
		}
	}
}

func TestWaitForLogReportsIncompleteHistory(t *testing.T) {
	f := newAPIFixture(t)
	eng := &stubLogsEngine{body: windowLogBody, readErr: errors.New("task skipped")}
	svc := api.NewService(f.q, eng, nil)
	// The fallback engine has no following read: ServiceLogs(follow) returns
	// the same short body, which ends at once.
	res, err := svc.Logs(t.Context(), f.readIdent, f.appIDString, api.LogsInput{WaitFor: "never", Since: "2026-11-20T13:00:00Z", Timeout: 2})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if w := res.(api.LogWaitResult); w.Found || !w.Incomplete {
		t.Fatalf("a broken history read must be reported: %+v", w)
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
	if el := time.Since(start); el < 900*time.Millisecond || el > 5*time.Second {
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
