//go:build integration

package observability

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestAppsCollectorEndToEnd(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })
	nginx := `events {} 
http { server { listen 80; location /metrics { if ($http_authorization != "Bearer apptok") { return 401; } default_type text/plain; return 200 "probe_metric 1\n"; } location = /big { if ($http_authorization != "Bearer apptok") { return 401; } default_type text/plain; alias /srv/big.txt; } } }`
	// One sample over the module's per-target sample_limit.
	var big strings.Builder
	for i := 0; i <= 5000; i++ {
		fmt.Fprintf(&big, "big_metric{i=\"%d\"} 1\n", i)
	}
	target, err := testcontainers.Run(ctx, "nginx:alpine", network.WithNetwork([]string{"tasks.krill-7"}, nw), testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(nginx), ContainerFilePath: "/etc/nginx/nginx.conf", FileMode: 0644}, testcontainers.ContainerFile{Reader: strings.NewReader(big.String()), ContainerFilePath: "/srv/big.txt", FileMode: 0644}), testcontainers.WithWaitStrategy(wait.ForLog("Configuration complete")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, target)
	ip, err := target.ContainerIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeReceiver{bodies: map[string][][]byte{}, auth: map[string]string{}}
	var mu sync.Mutex
	providerDown, wrongToken := false, false
	polls := 0
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	base := "http://" + testcontainers.HostInternal + ":" + strconv.Itoa(port)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_krill/alloy/apps" {
			rec.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		down, wrong := providerDown, wrongToken
		mu.Unlock()
		if down || r.Header.Get("Authorization") != "Bearer prov" {
			http.NotFound(w, r)
			return
		}
		token := "apptok"
		if wrong {
			token = "wrong"
		}
		module, err := RenderAppsModule([]AppTarget{
			{OrgID: 1, AppID: 7, EndpointID: 3, Org: "Acme", Project: "p", Env: "prod", App: "app", Port: 80, Path: "/metrics", Job: "relay", Token: token},
			{OrgID: 1, AppID: 7, EndpointID: 4, Org: "Acme", Project: "p", Env: "prod", App: "app", Port: 80, Path: "/big", Job: "big", Token: token},
		}, map[int64][]TaskNode{7: {{IP: ip, Node: "node-a"}}})
		if err != nil {
			http.Error(w, "module error", 500)
			return
		}
		mu.Lock()
		polls++
		mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(module)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	cfg, err := RenderAppsConfig(Settings{Enabled: true, Metrics: Target{URL: base + "/api/v1/push", User: "m", Password: "mpw"}}, base+AppsProviderPath)
	if err != nil {
		t.Fatal(err)
	}
	collector, err := testcontainers.Run(ctx, Image, network.WithNetwork(nil, nw), testcontainers.WithHostPortAccess(port), hostTunnelEntrypoint(port), testcontainers.WithFiles(
		testcontainers.ContainerFile{Reader: bytes.NewReader(cfg), ContainerFilePath: configPath, FileMode: 0644},
		testcontainers.ContainerFile{Reader: strings.NewReader("prov"), ContainerFilePath: secretsDir + appsProviderFile, FileMode: 0400},
		testcontainers.ContainerFile{Reader: strings.NewReader("mpw"), ContainerFilePath: secretsDir + metricsSecretFile, FileMode: 0400},
	), testcontainers.WithCmd(nodeArgs()...), testcontainers.WithWaitStrategy(wait.ForLog("waiting for host tunnel").WithStartupTimeout(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, collector)
	exec := func(ctx context.Context, _ string, cmd []string, out io.Writer) error {
		code, r, err := collector.Exec(ctx, cmd, tcexec.Multiplexed())
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("exec exit %d", code)
		}
		_, err = io.Copy(out, r)
		return err
	}
	until := func(timeout time.Duration, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(time.Second)
		}
		logs, _ := collector.Logs(ctx)
		if logs != nil {
			defer logs.Close()
			b, _ := io.ReadAll(logs)
			t.Log(string(b))
		}
		t.Fatal("collector did not reach expected state")
	}
	until(90*time.Second, func() bool { ok, _ := rec.seen("/api/v1/push", "probe_metric"); return ok })
	for _, needle := range []string{"krill_node", "node-a", "krill_app", "krill_app_id", "krill_org_id", "relay"} {
		if ok, _ := rec.seen("/api/v1/push", needle); !ok {
			t.Errorf("remote_write lacks %q", needle)
		}
	}
	if ok, _ := rec.seen("/api/v1/push", "__tmp_krill_node"); ok {
		t.Error("internal label leaked")
	}
	if _, auth := rec.seen("/api/v1/push", "probe_metric"); auth != "Basic "+basic("m", "mpw") {
		t.Fatalf("remote_write auth=%q", auth)
	}
	until(15*time.Second, func() bool {
		h, eps, err := ReadAppsStatus(ctx, exec, 7, []int64{3})
		return err == nil && h.Healthy && len(eps) == 1 && len(eps[0].Targets) == 1 && eps[0].Targets[0].Health == "up" && eps[0].Targets[0].Node == "node-a"
	})
	// The oversized endpoint fails on its own and says why; the other keeps working.
	until(45*time.Second, func() bool {
		_, eps, err := ReadAppsStatus(ctx, exec, 7, []int64{4})
		return err == nil && len(eps) == 1 && len(eps[0].Targets) == 1 && eps[0].Targets[0].Health == "down" && strings.Contains(eps[0].Targets[0].LastError, "sample limit")
	})
	if ok, _ := rec.seen("/api/v1/push", "big_metric"); ok {
		t.Error("samples over sample_limit reached remote_write")
	}
	mu.Lock()
	providerDown = true
	beforePolls := polls
	mu.Unlock()
	rec.mu.Lock()
	rec.bodies["/api/v1/push"] = nil
	rec.mu.Unlock()
	// Wait beyond one failed 30s provider poll, then require a fresh sample.
	timer := time.NewTimer(40 * time.Second)
	<-timer.C
	rec.mu.Lock()
	rec.bodies["/api/v1/push"] = nil
	rec.mu.Unlock()
	until(50*time.Second, func() bool { ok, _ := rec.seen("/api/v1/push", "probe_metric"); return ok })
	mu.Lock()
	if polls != beforePolls {
		t.Error("provider succeeded while disabled")
	}
	providerDown = false
	wrongToken = true
	mu.Unlock()
	until(90*time.Second, func() bool {
		_, eps, err := ReadAppsStatus(ctx, exec, 7, []int64{3})
		return err == nil && len(eps) == 1 && len(eps[0].Targets) == 1 && eps[0].Targets[0].Health == "down" && strings.Contains(eps[0].Targets[0].LastError, "401")
	})
}

func TestAppsCollectorColdStartWithoutProvider(t *testing.T) {
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	cfg, err := RenderAppsConfig(Settings{Metrics: Target{URL: "http://127.0.0.1:1/write"}}, "http://"+testcontainers.HostInternal+":"+strconv.Itoa(port)+AppsProviderPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := testcontainers.Run(ctx, Image, testcontainers.WithHostPortAccess(port), hostTunnelEntrypoint(port), testcontainers.WithFiles(testcontainers.ContainerFile{Reader: bytes.NewReader(cfg), ContainerFilePath: configPath, FileMode: 0644}, testcontainers.ContainerFile{Reader: strings.NewReader("prov"), ContainerFilePath: secretsDir + appsProviderFile, FileMode: 0400}), testcontainers.WithCmd(nodeArgs()...), testcontainers.WithWaitStrategy(wait.ForLog("waiting for host tunnel")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, c)
	deadline := time.Now().Add(time.Minute)
	state, err := c.State(ctx)
	for err == nil && state.Running && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		state, err = c.State(ctx)
	}
	if err != nil || state.Running || state.ExitCode == 0 {
		t.Fatalf("cold start should fail: %+v %v", state, err)
	}
}

func TestAppsModuleWithUnsafeNamesLoads(t *testing.T) {
	ctx := context.Background()
	targets := sampleTargets()
	targets[0].Org = `Org "x"`
	targets[1].App = "db узел"
	module, err := RenderAppsModule(targets, map[int64][]TaskNode{9: {{IP: "10.0.0.2", Node: "a$1"}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := `prometheus.remote_write "default" {
 endpoint {
 url = "http://127.0.0.1:1/write"
 }
}
local.file "module" {
 filename = "/tmp/module.alloy"
 is_secret = true
}
import.string "apps" {
 content = local.file.module.content
}
apps.targets "default" {
 forward_to = [prometheus.remote_write.default.receiver]
}
`
	c, err := testcontainers.Run(ctx, Image, testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(cfg), ContainerFilePath: configPath, FileMode: 0644}, testcontainers.ContainerFile{Reader: bytes.NewReader(module), ContainerFilePath: "/tmp/module.alloy", FileMode: 0644}), testcontainers.WithCmd(nodeArgs()...), testcontainers.WithWaitStrategy(wait.ForLog("now listening for http traffic")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, c)
	h, _, err := ReadAppsStatus(ctx, func(ctx context.Context, _ string, cmd []string, out io.Writer) error {
		code, r, err := c.Exec(ctx, cmd, tcexec.Multiplexed())
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("exec: %d", code)
		}
		_, err = io.Copy(out, r)
		return err
	}, 9, nil)
	if err != nil || !h.Healthy {
		t.Fatalf("unsafe names broke module: %+v %v", h, err)
	}
}

// testcontainers opens host-port tunnels only after its ready hook. Signal
// readiness before starting Alloy, whose very first provider poll must work.
func hostTunnelEntrypoint(port int) testcontainers.CustomizeRequestOption {
	return testcontainers.WithEntrypoint("bash", "-c", `echo 'waiting for host tunnel'
for i in {1..100}; do
 if (exec 3<>/dev/tcp/host.testcontainers.internal/"$1") 2>/dev/null; then
 shift
 exec /bin/alloy "$@"
 fi
 sleep 0.1
done
exit 7`, "krill-app-test", strconv.Itoa(port))
}
