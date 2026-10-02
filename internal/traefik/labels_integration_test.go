//go:build integration

package traefik

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestHiddenMetricsPathsOnRealTraefik runs the pinned Traefik in front of a
// backend that answers every path, and checks that the domain rule keeps the
// spellings a lenient backend would still route to its metrics handler off
// the domain, while other paths keep working.
func TestHiddenMetricsPathsOnRealTraefik(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })

	nginx := `events {}
http { server { listen 80; location / { default_type text/plain; return 200 "backend-ok"; } } }`
	backend, err := testcontainers.Run(ctx, "nginx:alpine",
		network.WithNetwork([]string{"backend"}, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(nginx), ContainerFilePath: "/etc/nginx/nginx.conf", FileMode: 0644}),
		testcontainers.WithWaitStrategy(wait.ForLog("Configuration complete")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, backend)

	rule := domainRule("a.example", nil, []string{"/metrics", "/actuator/prometheus"})
	dynamic := fmt.Sprintf(`http:
  routers:
    app:
      rule: %q
      entryPoints: [web]
      service: app
  services:
    app:
      loadBalancer:
        servers:
          - url: "http://backend:80"
`, rule)
	gw, err := testcontainers.Run(ctx, "traefik:"+TraefikVersion,
		network.WithNetwork(nil, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(dynamic), ContainerFilePath: "/etc/traefik/dynamic.yml", FileMode: 0644}),
		testcontainers.WithCmd("--entrypoints.web.address=:80", "--providers.file.filename=/etc/traefik/dynamic.yml"),
		testcontainers.WithExposedPorts("80/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("80/tcp")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, gw)
	endpoint, err := gw.PortEndpoint(ctx, "80/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}

	served := func(path string) bool {
		t.Helper()
		req, err := http.NewRequest("GET", endpoint+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "a.example"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == 200 && string(body) == "backend-ok"
	}
	// The file provider loads asynchronously; wait until the router serves.
	deadline := time.Now().Add(30 * time.Second)
	for !served("/") {
		if time.Now().After(deadline) {
			t.Fatal("traefik never routed to the backend")
		}
		time.Sleep(500 * time.Millisecond)
	}

	for _, p := range []string{"/", "/api", "/metricsfoo", "/metrics-old", "/actuator/health", "/prefix/metrics"} {
		if !served(p) {
			t.Errorf("%s: not served, but it is not a metrics path", p)
		}
	}
	for _, p := range []string{
		"/metrics", "/metrics/", "/metrics/x", "/METRICS", "/Metrics/x",
		"/metrics;jsessionid=1", "/%6Detrics", "/%4Detrics",
		"/actuator/prometheus", "/ACTUATOR/Prometheus", "/actuator/prometheus;x",
	} {
		if served(p) {
			t.Errorf("%s: served through the domain", p)
		}
	}
}

// runEcho starts an nginx on nw under alias that answers every path with body.
func runEcho(t *testing.T, ctx context.Context, nw *testcontainers.DockerNetwork, alias, body string) {
	t.Helper()
	conf := fmt.Sprintf(`events {}
http { server { listen 80; location / { default_type text/plain; return 200 %q; } } }`, body)
	c, err := testcontainers.Run(ctx, "nginx:alpine",
		network.WithNetwork([]string{alias}, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(conf), ContainerFilePath: "/etc/nginx/nginx.conf", FileMode: 0644}),
		testcontainers.WithWaitStrategy(wait.ForLog("Configuration complete")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, c)
}

// TestSharedHostOnRealTraefik puts a catch-all app (with a hidden metrics path,
// which makes its rule the LONGER one) and a /api app on one host, and checks
// that the explicit priorities and the segment rule route every path to the
// right backend. Without explicit priorities Traefik would rank by rule length
// and send /api to the catch-all.
func TestSharedHostOnRealTraefik(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })
	runEcho(t, ctx, nw, "web", "web")
	runEcho(t, ctx, nw, "api", "api")

	webRule := domainRule("m.example", nil, []string{"/metrics"})
	apiRule := domainRule("m.example", []string{"/api"}, nil)
	if len(webRule) <= len(apiRule) {
		t.Fatalf("precondition: the catch-all rule must be the longer one (%d vs %d)", len(webRule), len(apiRule))
	}
	dynamic := fmt.Sprintf(`http:
  routers:
    web:
      rule: %q
      priority: %d
      entryPoints: [web]
      service: web
    api:
      rule: %q
      priority: %d
      entryPoints: [web]
      service: api
  services:
    web:
      loadBalancer:
        servers:
          - url: "http://web:80"
    api:
      loadBalancer:
        servers:
          - url: "http://api:80"
`, webRule, PriorityCatchAll, apiRule, PriorityPaths)
	gw, err := testcontainers.Run(ctx, "traefik:"+TraefikVersion,
		network.WithNetwork(nil, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(dynamic), ContainerFilePath: "/etc/traefik/dynamic.yml", FileMode: 0644}),
		testcontainers.WithCmd("--entrypoints.web.address=:80", "--providers.file.filename=/etc/traefik/dynamic.yml"),
		testcontainers.WithExposedPorts("80/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("80/tcp")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, gw)
	endpoint, err := gw.PortEndpoint(ctx, "80/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", endpoint+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "m.example"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if code, _ := get("/"); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("traefik never routed to the backends")
		}
		time.Sleep(500 * time.Millisecond)
	}

	for path, want := range map[string]string{
		"/api": "api", "/api/": "api", "/api/v1/x": "api",
		"/": "web", "/apix": "web", "/apiary": "web", "/app/api": "web",
	} {
		if code, body := get(path); code != 200 || body != want {
			t.Errorf("%s: got %d %q, want 200 %q", path, code, body, want)
		}
	}
	if code, _ := get("/metrics"); code != 404 {
		t.Errorf("/metrics: got %d, want 404 (hidden on the catch-all, not claimed by /api)", code)
	}
}
