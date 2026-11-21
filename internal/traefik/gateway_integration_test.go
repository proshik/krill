//go:build integration

package traefik

import (
	"bufio"
	"context"
	"encoding/json"
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

// streamBackend answers /stream?silence=N with "head\n", a flush, N seconds of
// silence and "tail\n"; any other path is a 404.
const streamBackend = `package main

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func main() {
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("silence"))
		fmt.Fprint(w, "head\n")
		w.(http.Flusher).Flush()
		time.Sleep(time.Duration(n) * time.Second)
		fmt.Fprint(w, "tail\n")
	})
	http.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	http.ListenAndServe(":80", nil)
}
`

// TestGatewayTimeoutsAndAccessLogOnRealTraefik runs the pinned Traefik with the
// gateway's own timeout and filtered access-log arguments and checks what an
// app with a long response can rely on: a response that stays silent longer
// than the entrypoint's readTimeout (60s) is still delivered whole, and the
// access log records it — status, bytes sent to the client and duration —
// while a fast successful request is left out.
func TestGatewayTimeoutsAndAccessLogOnRealTraefik(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })

	backend, err := testcontainers.Run(ctx, "golang:1.26.1-alpine",
		network.WithNetwork([]string{"app"}, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(streamBackend), ContainerFilePath: "/src/main.go", FileMode: 0644}),
		testcontainers.WithEnv(map[string]string{"GOFLAGS": "-mod=mod", "CGO_ENABLED": "0"}),
		testcontainers.WithCmd("go", "run", "/src/main.go"),
		testcontainers.WithExposedPorts("80/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("80/tcp").WithStartupTimeout(3*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, backend)

	dynamic := `http:
  routers:
    app:
      rule: "Host(` + "`a.example`" + `)"
      entryPoints: [web]
      service: app
  services:
    app:
      loadBalancer:
        servers:
          - url: "http://app:80"
`
	args := []string{"--entrypoints.web.address=:80", "--providers.file.filename=/etc/traefik/dynamic.yml"}
	for _, a := range gatewayTimeoutArgs {
		if !strings.HasPrefix(a, "--entrypoints.websecure.") {
			args = append(args, a)
		}
	}
	args = append(args, accessLogArgs(AccessLogFiltered)...)
	gw, err := testcontainers.Run(ctx, "traefik:"+TraefikVersion,
		network.WithNetwork(nil, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader(dynamic), ContainerFilePath: "/etc/traefik/dynamic.yml", FileMode: 0644}),
		testcontainers.WithCmd(args...),
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

	h1 := &http.Client{}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	h2 := &http.Client{Transport: &http.Transport{Protocols: &p}}
	do := func(c *http.Client, path string) (int, string, error) {
		req, err := http.NewRequest("GET", endpoint+path, nil)
		if err != nil {
			return 0, "", err
		}
		req.Host = "a.example"
		resp, err := c.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err == nil && c == h2 && resp.ProtoMajor != 2 {
			err = fmt.Errorf("spoke %s, want HTTP/2", resp.Proto)
		}
		return resp.StatusCode, string(body), err
	}
	get := func(path string) (int, string, error) { return do(h1, path) }
	deadline := time.Now().Add(30 * time.Second)
	for {
		if code, body, err := get("/ok"); err == nil && code == 200 && body == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("traefik never routed to the backend")
		}
		time.Sleep(500 * time.Millisecond)
	}

	if code, _, err := get("/missing"); err != nil || code != 404 {
		t.Fatalf("/missing: %d %v", code, err)
	}
	// Both protocols at once: browsers and curl speak HTTP/2 to the TLS
	// entrypoint, scripts often HTTP/1.1.
	const silence = 75 // seconds, past the 60s readTimeout
	type result struct {
		proto string
		code  int
		body  string
		err   error
	}
	results := make(chan result, 2)
	for proto, c := range map[string]*http.Client{"h1": h1, "h2": h2} {
		go func() {
			code, body, err := do(c, "/stream?silence=75&proto="+proto)
			results <- result{proto, code, body, err}
		}()
	}
	for range 2 {
		r := <-results
		if r.err != nil || r.code != 200 || r.body != "head\ntail\n" {
			t.Errorf("%s: long silent response cut: status %d, body %q, err %v", r.proto, r.code, r.body, r.err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	type entry struct {
		RequestPath           string
		DownstreamStatus      int
		DownstreamContentSize int
		Duration              int64
	}
	var entries []entry
	deadline = time.Now().Add(10 * time.Second)
	for {
		entries = entries[:0]
		logs, err := gw.Logs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(logs)
		for sc.Scan() {
			var e entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.RequestPath != "" {
				entries = append(entries, e)
			}
		}
		logs.Close()
		if len(entries) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	byPath := map[string]entry{}
	for _, e := range entries {
		// The readiness loop above may hit /ok before the router loads; those
		// 404s are rightly logged. A fast 200 must not be.
		if e.RequestPath == "/ok" && e.DownstreamStatus == 200 {
			t.Error("filtered access log recorded a fast successful request")
		}
		byPath[e.RequestPath] = e
	}
	if e, ok := byPath["/missing"]; !ok || e.DownstreamStatus != 404 {
		t.Errorf("404 not logged: %+v", byPath)
	}
	for _, proto := range []string{"h1", "h2"} {
		e, ok := byPath["/stream?silence=75&proto="+proto]
		if !ok {
			t.Fatalf("long %s request not logged: %+v", proto, entries)
		}
		if e.DownstreamStatus != 200 || e.DownstreamContentSize != len("head\ntail\n") || time.Duration(e.Duration) < (silence-1)*time.Second {
			t.Fatalf("long %s request entry = %+v", proto, e)
		}
	}
}
