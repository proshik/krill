//go:build integration

package observability

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// alloyImage is Image without the digest: testcontainers resolves either, and
// the tag keeps the log output readable.
const alloyImage = "grafana/alloy:v1.19.2"

func TestNodeConfigValidates(t *testing.T) {
	nodes := fullNodes()
	for _, s := range []Settings{fullSettings(), {Metrics: Target{URL: "http://p:9090/api/v1/write"}}, {Logs: Target{URL: "http://l:3100/loki/api/v1/push", User: "u"}}} {
		cfg, err := RenderNodeConfig(s, nodes)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		c, err := testcontainers.Run(ctx, alloyImage,
			testcontainers.WithFiles(testcontainers.ContainerFile{Reader: bytes.NewReader(cfg), ContainerFilePath: "/tmp/c.alloy", FileMode: 0o644}),
			testcontainers.WithCmd("validate", "/tmp/c.alloy"),
			testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(60*time.Second)),
		)
		if err != nil {
			t.Fatalf("run validate: %v", err)
		}
		st, _ := c.State(ctx)
		logs, _ := c.Logs(ctx)
		out, _ := io.ReadAll(logs)
		_ = c.Terminate(ctx)
		if st == nil || st.ExitCode != 0 {
			t.Fatalf("alloy validate failed:\n%s\nconfig:\n%s", out, cfg)
		}
	}
}

// TestNodeConfigWithNodesLoads goes further than TestNodeConfigValidates: as
// CLAUDE.md's Alloy gotcha notes, `alloy validate` does not catch every
// class of error — confirmed here the hard way, since it accepts a
// single-backslash regex (e.g. "node-1\.ru") that `alloy run` then rejects
// at load with "unknown escape sequence" (verified manually against this same
// pinned image before writing RenderNodeConfig's escaping). This test
// actually loads the rendered config with the real per-node relabel rules and
// waits for the agent to report itself running, so a regression that drops
// the backslash-doubling in alloyRegexLiteral fails here even though it would
// still pass `alloy validate`. Metrics-only avoids also needing the docker
// socket mount TestNodeAgentShipsMetricsAndLogs uses, but prometheus.exporter.unix
// still needs the host /proc, /sys and / binds regardless of the node mapping.
func TestNodeConfigWithNodesLoads(t *testing.T) {
	s := Settings{Enabled: true, Metrics: Target{URL: "http://p:9090/api/v1/write"}}
	cfg, err := RenderNodeConfig(s, fullNodes())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := testcontainers.Run(ctx, alloyImage,
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: bytes.NewReader(cfg), ContainerFilePath: configPath, FileMode: 0o644}),
		testcontainers.WithCmd(nodeArgs()...),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, "/proc:/host/proc:ro", "/sys:/host/sys:ro", "/:/host/root:ro")
		}),
		testcontainers.WithWaitStrategy(wait.ForLog("now listening for http traffic").WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		logsErr := "(no container)"
		if c != nil {
			logs, _ := c.Logs(ctx)
			out, _ := io.ReadAll(logs)
			logsErr = string(out)
		}
		t.Fatalf("agent did not start with the node relabel rules: %v\nconfig:\n%s\nlogs:\n%s", err, cfg, logsErr)
	}
	defer func() { _ = c.Terminate(ctx) }()
	logs, _ := c.Logs(ctx)
	out, _ := io.ReadAll(logs)
	if bytes.Contains(out, []byte("unknown escape sequence")) || bytes.Contains(out, []byte("level=error")) {
		t.Errorf("agent logged errors with the node relabel rules:\n%s", out)
	}
}

// fakeReceiver records the requests Alloy sends to it.
type fakeReceiver struct {
	mu     sync.Mutex
	bodies map[string][][]byte // path -> bodies
	auth   map[string]string   // path -> last Authorization header
}

func (f *fakeReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies[r.URL.Path] = append(f.bodies[r.URL.Path], b)
	f.auth[r.URL.Path] = r.Header.Get("Authorization")
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeReceiver) seen(path string, needle string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bodies[path] {
		if bytes.Contains(b, []byte(needle)) {
			return true, f.auth[path]
		}
	}
	return false, f.auth[path]
}

func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func TestNodeAgentShipsMetricsAndLogs(t *testing.T) {
	rec := &fakeReceiver{bodies: map[string][][]byte{}, auth: map[string]string{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: rec}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	base := "http://" + testcontainers.HostInternal + ":" + strconv.Itoa(port)

	s := Settings{
		Enabled: true,
		Metrics: Target{URL: base + "/api/v1/push", User: "m", Password: "mpw"},
		Logs:    Target{URL: base + "/loki/api/v1/push", User: "l", Password: "lpw"},
	}
	cfg, err := RenderNodeConfig(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A labelled container whose output the agent must pick up.
	app, err := testcontainers.Run(ctx, "busybox:1.36",
		testcontainers.WithCmd("sh", "-c", "while true; do echo krill-log-probe; sleep 1; done"),
		testcontainers.WithLabels(ContainerLabels(AppIdentity{OrgID: 1, Org: "Acme", Project: "shop", Env: "prod", AppID: 9, App: "web"})),
	)
	if err != nil {
		t.Fatalf("app container: %v", err)
	}
	t.Cleanup(func() { _ = app.Terminate(context.Background()) })

	agent, err := testcontainers.Run(ctx, alloyImage,
		testcontainers.WithHostPortAccess(port),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{Reader: bytes.NewReader(cfg), ContainerFilePath: configPath, FileMode: 0o644},
			testcontainers.ContainerFile{Reader: strings.NewReader("mpw"), ContainerFilePath: secretsDir + metricsSecretFile, FileMode: 0o400},
			testcontainers.ContainerFile{Reader: strings.NewReader("lpw"), ContainerFilePath: secretsDir + logsSecretFile, FileMode: 0o400},
		),
		testcontainers.WithCmd(nodeArgs()...),
		testcontainers.WithConfigModifier(func(c *container.Config) { c.Hostname = "probe-node" }),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, "/proc:/host/proc:ro", "/sys:/host/sys:ro", "/:/host/root:ro", "/var/run/docker.sock:/var/run/docker.sock:ro")
		}),
		testcontainers.WithWaitStrategy(wait.ForLog("now listening for http traffic").WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	t.Cleanup(func() { _ = agent.Terminate(context.Background()) })

	wantAuthM := "Basic " + basic("m", "mpw")
	wantAuthL := "Basic " + basic("l", "lpw")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		// Both bodies are compressed protobuf, so this proves delivery and
		// credentials; the labels are checked against a real Loki in Task 13.
		mSeen, mAuth := rec.seen("/api/v1/push", "")
		lSeen, lAuth := rec.seen("/loki/api/v1/push", "")
		if mSeen && lSeen {
			if mAuth != wantAuthM || lAuth != wantAuthL {
				t.Fatalf("auth headers: metrics %q, logs %q", mAuth, lAuth)
			}
			break
		}
		if time.Now().After(deadline) {
			logs, _ := agent.Logs(ctx)
			out, _ := io.ReadAll(logs)
			t.Fatalf("agent sent nothing (metrics=%v logs=%v); agent log:\n%s", mSeen, lSeen, out)
		}
		time.Sleep(2 * time.Second)
	}
	logs, _ := agent.Logs(ctx)
	out, _ := io.ReadAll(logs)
	if bytes.Contains(out, []byte("level=error")) {
		t.Errorf("agent logged errors:\n%s", out)
	}
}
