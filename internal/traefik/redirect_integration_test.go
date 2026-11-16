//go:build integration

package traefik

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestRedirectToPrimaryOnRealTraefik puts the labels AppLabels writes on a real
// container and lets the pinned Traefik read them through its Docker provider
// (the label format is the one the Swarm provider reads). The "le" resolver
// points at an unreachable CA, so TLS routers serve Traefik's default
// certificate and nothing leaves the machine, while the ACME HTTP-01 router is
// still installed exactly as in production.
func TestRedirectToPrimaryOnRealTraefik(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })

	labels := AppLabels("krill-1", []Domain{
		{Host: "app.example", TLS: true, Exposed: true, Primary: true},
		{Host: "old.example", TLS: true, RedirectToPrimary: true, Paths: []string{"/only"}, BasicAuthUsers: []string{"u:$2a$10$x"}},
		{Host: "plain.example", RedirectToPrimary: true},
	}, 80, nw.Name, []string{"/metrics"})
	nginx := `events {}
http { server { listen 80; location / { default_type text/plain; return 200 "backend-ok"; } } }`
	backend, err := testcontainers.Run(ctx, "nginx:alpine",
		network.WithNetwork([]string{"backend"}, nw),
		testcontainers.WithLabels(labels),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(nginx), ContainerFilePath: "/etc/nginx/nginx.conf", FileMode: 0644}),
		testcontainers.WithWaitStrategy(wait.ForLog("Configuration complete")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, backend)

	gw, err := testcontainers.Run(ctx, "traefik:"+TraefikVersion,
		network.WithNetwork(nil, nw),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, "/var/run/docker.sock:/var/run/docker.sock:ro")
		}),
		testcontainers.WithCmd(
			"--entrypoints.web.address=:80", "--entrypoints.websecure.address=:443",
			"--providers.docker.exposedByDefault=false",
			"--certificatesresolvers.le.acme.storage=/tmp/acme.json",
			"--certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
			"--certificatesresolvers.le.acme.caserver=https://127.0.0.1:9/directory",
		),
		testcontainers.WithExposedPorts("80/tcp", "443/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("80/tcp")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, gw)
	httpEP, err := gw.PortEndpoint(ctx, "80/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	httpsEP, err := gw.PortEndpoint(ctx, "443/tcp", "https")
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // Traefik's default cert
	}
	do := func(method, endpoint, host, path string) (int, string, string) {
		t.Helper()
		req, err := http.NewRequest(method, endpoint+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if code, _, _ := do("GET", httpsEP, "app.example", "/"); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("traefik never routed to the backend")
		}
		time.Sleep(500 * time.Millisecond)
	}

	cases := []struct {
		method, ep, host, path string
		code                   int
		location               string
	}{
		{"GET", httpEP, "old.example", "/a/b?x=1&y=%2F", 301, "https://app.example/a/b?x=1&y=%2F"},
		{"GET", httpsEP, "old.example", "/a/b?x=1", 301, "https://app.example/a/b?x=1"},
		{"POST", httpsEP, "old.example", "/form", 308, "https://app.example/form"},
		{"HEAD", httpEP, "old.example", "/", 308, "https://app.example/"},
		// No paths, no basic-auth on the redirecting domain; hidden paths bounce to the primary.
		{"GET", httpsEP, "old.example", "/metrics", 301, "https://app.example/metrics"},
		{"GET", httpEP, "plain.example", "/p?q=1", 301, "https://app.example/p?q=1"},
		// The primary's own rules still apply after the redirect.
		{"GET", httpsEP, "app.example", "/metrics", 404, ""},
		// The primary's own HTTP->HTTPS hop is the pre-existing temporary redirectscheme.
		{"GET", httpEP, "app.example", "/x", 302, "https://app.example/x"},
	}
	for _, c := range cases {
		code, loc, _ := do(c.method, c.ep, c.host, c.path)
		t.Logf("%s %s://%s%s -> %d Location=%q", c.method, strings.SplitN(c.ep, ":", 2)[0], c.host, c.path, code, loc)
		if code != c.code || loc != c.location {
			t.Errorf("%s %s%s: got %d %q, want %d %q", c.method, c.host, c.path, code, loc, c.code, c.location)
		}
	}
	// HTTP-01: the challenge path is answered by Traefik's ACME router, not redirected.
	code, loc, _ := do("GET", httpEP, "old.example", "/.well-known/acme-challenge/token")
	t.Logf("GET http://old.example/.well-known/acme-challenge/token -> %d Location=%q", code, loc)
	if code == 301 || code == 308 || loc != "" {
		t.Errorf("ACME challenge redirected: %d %q", code, loc)
	}
}
