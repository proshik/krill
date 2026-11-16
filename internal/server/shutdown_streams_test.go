package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// TestMCPStreamEndsOnShutdown pins the cause of a self-update stop that hit
// its deadline: an MCP client's standalone GET /mcp stream stays open until
// the client goes away, and http.Server.Shutdown waits for it. With
// StopStreams registered on the server, Shutdown must finish promptly and the
// stream must end cleanly while the client is still connected.
func TestMCPStreamEndsOnShutdown(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	hub := deploy.NewLogHub()
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", AgentAPIEnabled: true}
	app := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	app.SetAPI(api.NewAuthenticator(q, orgSvc), api.NewService(q, nil, nil))

	uid := mkUser(t, q, "mcp-shutdown@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), uid, "MCPShutdownOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token := issueAPIToken(t, q, uid, o.ID, api.LevelRead)

	router := app.Router()
	streamOpen := make(chan struct{}, 1)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/mcp" {
			streamOpen <- struct{}{}
		}
		router.ServeHTTP(w, r)
	}))
	ts.Config.RegisterOnShutdown(app.StopStreams)
	ts.Start()
	defer ts.Close()

	post := func(body, sid string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
			req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	initResp := post(mcpInitializeBody, "")
	sid := initResp.Header.Get("Mcp-Session-Id")
	if initResp.StatusCode != http.StatusOK || sid == "" {
		t.Fatalf("initialize: status %d, session %q", initResp.StatusCode, sid)
	}
	if resp := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, sid); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized notification: status %d", resp.StatusCode)
	}

	// On failure the client hangs up first (defers run last-in first-out):
	// ts.Close waits for every handler, the stuck stream included.
	clientCtx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	streamDone := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(clientCtx, http.MethodGet, ts.URL+"/mcp", nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Mcp-Session-Id", sid)
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			streamDone <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		streamDone <- err
	}()
	select {
	case <-streamOpen:
	case <-time.After(10 * time.Second):
		t.Fatal("the GET /mcp stream never reached the server")
	}
	// Let the SDK get the stream into its wait.
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-streamDone:
		t.Fatalf("the stream ended before shutdown (err=%v); the test proves nothing", err)
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := ts.Config.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with an open MCP stream: %v after %s", err, time.Since(start))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %s; the MCP stream should end at once", d)
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("the stream should end cleanly, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the client's stream did not end after Shutdown")
	}
}
