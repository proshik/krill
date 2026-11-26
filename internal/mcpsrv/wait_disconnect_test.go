package mcpsrv_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/mcpsrv"
	"github.com/proshik/krill/internal/org"
)

// blockingFollow is an engine whose live log stream stays open, silent,
// until the read's context ends — and reports when it opened and ended.
type blockingFollow struct {
	docker.Engine
	opened, ended chan struct{}
}

func (e *blockingFollow) ServiceLogsWindow(ctx context.Context, _ string, w docker.LogWindow) (io.ReadCloser, error) {
	if !w.Follow {
		return io.NopCloser(strings.NewReader("")), nil
	}
	pr, pw := io.Pipe()
	close(e.opened)
	go func() {
		<-ctx.Done()
		pw.Close()
		close(e.ended)
	}()
	return pr, nil
}

// TestLogWaitEndsWhenTheCallingPOSTGoesAway drives krill_app_logs with
// wait_for over a real stateful MCP session and drops only the tools/call
// POST: the tool runs on the session's context, which outlives that request,
// so only the registration's requestScoped wrapper can stop the Docker follow
// (and free its wait slot) before the timeout.
func TestLogWaitEndsWhenTheCallingPOSTGoesAway(t *testing.T) {
	f := newSmokeFixture(t)
	q := db.New(f.pool)
	orgSvc := org.NewService(q)
	proj, err := orgSvc.CreateProject(t.Context(), f.orgID, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := orgSvc.CreateEnvironment(t.Context(), proj.ID, "e")
	if err != nil {
		t.Fatal(err)
	}
	app, err := q.CreateApplication(t.Context(), db.CreateApplicationParams{
		EnvironmentID: env.ID, Name: "a", Image: "nginx", Tag: "alpine", Domain: "a.example.test", Port: 80, SourceType: "image",
	})
	if err != nil {
		t.Fatal(err)
	}
	eng := &blockingFollow{opened: make(chan struct{}), ended: make(chan struct{})}
	svc := api.NewService(q, eng, nil)
	ts := httptest.NewServer(withBearerAuth(f.authn, mcpsrv.New(svc, noIdleTimeout).Handler()))
	defer ts.Close()

	_, sid := postJSONRPC(t, ts.URL,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"v0"}}}`, f.token, "")
	if sid == "" {
		t.Fatal("no session")
	}

	ctx, drop := context.WithCancel(context.Background())
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"krill_app_logs","arguments":{"app":"` +
		strconv.FormatInt(app.ID, 10) + `","wait_for":"never","timeout":60}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Mcp-Session-Id", sid)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-eng.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never opened its follow")
	}
	drop() // the caller goes away; the session stays
	select {
	case <-eng.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the Docker follow outlived the POST that asked for it")
	}
}
