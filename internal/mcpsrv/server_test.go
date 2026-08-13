package mcpsrv_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/mcpsrv"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

// TestToolListMatchesRegistry pins the exact set of MCP tools this adapter
// exposes. The tool list IS the adapter's entire security surface — an MCP
// client can call exactly what is registered here and nothing else — so a
// stray thirteenth registration (or a typo'd name that silently duplicates
// one of the twelve) must fail a test, not slip into production unnoticed.
// The list only ever changes together with this test, deliberately.
func TestToolListMatchesRegistry(t *testing.T) {
	want := []string{
		"krill_app_logs", "krill_app_status", "krill_deploy", "krill_deployment_status",
		"krill_deployments", "krill_list_apps", "krill_list_env", "krill_rebuild",
		"krill_reload", "krill_set_env", "krill_stop", "krill_whoami",
	}
	got := mcpsrv.ToolNames()
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool registry drift:\ngot  %v\nwant %v", got, want)
	}
}

// smokeFixture wires a real api.Service over a real (testcontainer) database
// with one org and one write-level token — everything the protocol smoke
// test needs to prove a tools/call genuinely reaches internal/api.Service
// rather than a canned in-package result.
type smokeFixture struct {
	authn   *api.Authenticator
	svc     *api.Service
	pool    *pgxpool.Pool
	orgID   int64
	orgName string
	token   string
}

// newSmokeFixture seeds a user + org (mirrors the pattern used by
// internal/server/api_middleware_test.go's issueAPIToken and
// internal/api/identity_integration_test.go's seedTokenFixture — duplicated
// here rather than imported, since both of those live in different test
// packages this one cannot reach). The org name is randomized-ish via the
// test name so the whoami assertion below can distinguish "the service
// looked up the real row" from "a handler returned some fixed string".
func newSmokeFixture(t *testing.T) smokeFixture {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	authSvc := auth.NewService(q)
	if err := authSvc.SeedAdmin(t.Context(), "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	orgSvc := org.NewService(q)
	u, err := q.GetUserByEmail(t.Context(), "admin@k.local")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	orgName := "mcp-smoke-" + t.Name()
	o, err := orgSvc.CreateOrg(t.Context(), u.ID, orgName)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	plain, prefix, hash, err := api.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := q.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		UserID: u.ID, OrgID: o.ID, Name: "smoke",
		TokenHash: hash, Prefix: prefix, Level: string(api.LevelWrite),
	}); err != nil {
		t.Fatalf("create api token: %v", err)
	}

	// Engine/deployer/hub are nil: krill_whoami touches only the database.
	svc := api.NewService(q, nil, nil, deploy.NewLogHub())
	return smokeFixture{
		authn:   api.NewAuthenticator(q, orgSvc),
		svc:     svc,
		pool:    pool,
		orgID:   o.ID,
		orgName: orgName,
		token:   plain,
	}
}

// withBearerAuth is a minimal stand-in for internal/server's RequireAPIToken:
// it authenticates the Authorization header and stashes the resolved
// Identity via api.WithIdentity before handing off to next — exactly what
// production wiring does, just without importing internal/server (which
// would import this package back, since internal/server mounts
// internal/mcpsrv's handler). A request with no or an invalid bearer token is
// rejected with 401 and never reaches next, matching RequireAPIToken's
// behavior closely enough for this smoke test's purposes.
func withBearerAuth(authn *api.Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ident, err := authn.Authenticate(r.Context(), strings.TrimPrefix(h, "Bearer "), time.Now())
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(api.WithIdentity(r.Context(), ident)))
	})
}

// rpcEnvelope is the minimal JSON-RPC 2.0 response envelope this test needs
// to unwrap: either a Result or an Error, never both.
type rpcEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// wireCallToolResult mirrors the wire shape of mcp.CallToolResult closely
// enough to read back what a tools/call response actually contained: whether
// the tool reported an error, and its text content.
type wireCallToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// postJSONRPC posts one JSON-RPC message to url with the given bearer token,
// optionally carrying an Mcp-Session-Id header (sid == "" omits it), and
// decodes the raw envelope. The Accept header requests both response formats
// per the streamable-HTTP spec, though this package's own Server.New always
// configures JSONResponse:true, so the body is always a single JSON object,
// never SSE.
func postJSONRPC(t *testing.T, url, body, token, sid string) (rpcEnvelope, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s: want 200, got %d", url, resp.StatusCode)
	}
	var env rpcEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return env, resp.Header.Get("Mcp-Session-Id")
}

// TestProtocolSmoke drives the real streamable-HTTP wire protocol —
// initialize, then tools/list, then one tools/call — through a plain
// net/http client against a genuine api.Service backed by a real database,
// exactly as an MCP client (not this package's own Go types) would see it.
// It proves two things the unit-level ToolNames test cannot:
//  1. The full HTTP/JSON-RPC round trip works end to end (session
//     negotiation, tool schema validation, response decoding).
//  2. A tools/call for krill_whoami genuinely reaches internal/api.Service —
//     the returned org id/name are read back from the database row this
//     test itself seeded, not a hardcoded value a stub could return.
func TestProtocolSmoke(t *testing.T) {
	f := newSmokeFixture(t)

	handler := withBearerAuth(f.authn, mcpsrv.New(f.svc).Handler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// initialize — no session id yet; the server mints one and returns it in
	// the Mcp-Session-Id response header.
	initEnv, sid := postJSONRPC(t, ts.URL,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"v0"}}}`, f.token, "")
	if initEnv.Error != nil {
		t.Fatalf("initialize: server error: %s", initEnv.Error.Message)
	}
	if sid == "" {
		t.Fatal("initialize: no Mcp-Session-Id returned")
	}

	// tools/list — asserts the live server actually advertises all twelve
	// registered tools over the wire, not just that ToolNames() says so.
	listEnv, _ := postJSONRPC(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, f.token, sid)
	if listEnv.Error != nil {
		t.Fatalf("tools/list: server error: %s", listEnv.Error.Message)
	}
	var listResult struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listEnv.Result, &listResult); err != nil {
		t.Fatalf("decode tools/list result: %v", err)
	}
	gotNames := make([]string, len(listResult.Tools))
	for i, tl := range listResult.Tools {
		gotNames[i] = tl.Name
	}
	sort.Strings(gotNames)
	wantSorted := append([]string(nil), mcpsrv.ToolNames()...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(gotNames, wantSorted) {
		t.Fatalf("tools/list mismatch:\ngot  %v\nwant %v", gotNames, wantSorted)
	}

	// tools/call krill_whoami — the load-bearing assertion: the result must
	// carry THIS test's own seeded org id/name, proving it came from a live
	// GetOrganization query against the row newSmokeFixture created, not a
	// stub or a value baked into the test itself.
	callEnv, _ := postJSONRPC(t, ts.URL,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"krill_whoami","arguments":{}}}`, f.token, sid)
	if callEnv.Error != nil {
		t.Fatalf("tools/call: protocol-level error: %s", callEnv.Error.Message)
	}
	var result wireCallToolResult
	if err := json.Unmarshal(callEnv.Result, &result); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	if result.IsError {
		t.Fatalf("krill_whoami reported a tool error: %+v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("krill_whoami returned no content")
	}
	var who api.WhoamiResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &who); err != nil {
		t.Fatalf("decode WhoamiResult: %v (text=%q)", err, result.Content[0].Text)
	}
	if who.OrgID != f.orgID {
		t.Fatalf("whoami org id = %d, want %d (this test's own seeded org) — the call did not genuinely reach the service", who.OrgID, f.orgID)
	}
	if who.OrgName != f.orgName {
		t.Fatalf("whoami org name = %q, want %q", who.OrgName, f.orgName)
	}
	if !who.CanWrite {
		t.Fatal("whoami: write-level token, owner role — CanWrite should be true")
	}
}

// startSession stands an MCP server up over svc behind the bearer-auth wrapper
// and completes the initialize handshake, returning the server URL and the
// negotiated session id — what every subsequent tools/call needs.
func startSession(t *testing.T, authn *api.Authenticator, svc *api.Service, token string) (string, string) {
	t.Helper()
	ts := httptest.NewServer(withBearerAuth(authn, mcpsrv.New(svc).Handler()))
	t.Cleanup(ts.Close)
	env, sid := postJSONRPC(t, ts.URL,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"v0"}}}`, token, "")
	if env.Error != nil {
		t.Fatalf("initialize: server error: %s", env.Error.Message)
	}
	if sid == "" {
		t.Fatal("initialize: no Mcp-Session-Id returned")
	}
	return ts.URL, sid
}

// callTool sends one tools/call and returns the decoded result. A protocol-level
// error (a malformed request, an unknown tool) fails the test outright: these
// tests are about what a *tool* reports, which arrives as a perfectly successful
// JSON-RPC response carrying isError.
func callTool(t *testing.T, url, token, sid, body string) wireCallToolResult {
	t.Helper()
	env, _ := postJSONRPC(t, url, body, token, sid)
	if env.Error != nil {
		t.Fatalf("tools/call: protocol-level error: %s", env.Error.Message)
	}
	var result wireCallToolResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	return result
}

// TestToolErrorReturnsAPIMessageVerbatim covers toolError's *api.Error branch:
// a service error authored for a caller passes through as its Message and
// NOTHING else.
//
// The assertion is deliberately an exact string equality on a single content
// block, not a substring check. Substring matching would still pass if someone
// later "improved" the message by appending the underlying cause — which is the
// precise regression this test exists to catch, because that cause can carry a
// DSN, a host path or a Go type name (see toolError's own doc comment). Any
// appended detail, or a second content block carrying it, breaks this test.
func TestToolErrorReturnsAPIMessageVerbatim(t *testing.T) {
	f := newSmokeFixture(t)
	url, sid := startSession(t, f.authn, f.svc, f.token)

	// A numeric reference that resolves to no row in this org: resolveApp
	// (internal/api/service.go) answers NotFound, an *api.Error.
	result := callTool(t, url, f.token, sid,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"krill_app_status","arguments":{"app":"999999"}}}`)

	if !result.IsError {
		t.Fatalf("want isError for an unresolvable app reference, got a success result: %+v", result.Content)
	}
	if len(result.Content) != 1 {
		t.Fatalf("want exactly one content block carrying the message, got %d: %+v", len(result.Content), result.Content)
	}
	const want = `no application "999999" in this organization`
	if got := result.Content[0].Text; got != want {
		t.Fatalf("api.Error message not passed through verbatim:\ngot  %q\nwant %q", got, want)
	}
}

// TestToolErrorHidesInternalCause covers toolError's other branch: an error
// that is NOT an *api.Error is an internal failure, and the caller must get the
// generic text plus a request id — never err.Error(), which here would read
// "whoami: get organization: closed pool" and in production can carry a DSN.
//
// The failure is injected honestly, with no fake service: the api.Service under
// test runs on its own pgxpool against the same database, and that pool is
// closed mid-session. Every query it makes then fails, api.Whoami wraps the
// failure with fmt.Errorf (internal/api/apps.go), and the tool handler sees a
// plain error. The authenticator keeps the fixture's live pool, so the bearer
// token still resolves and the request genuinely reaches the tool.
func TestToolErrorHidesInternalCause(t *testing.T) {
	f := newSmokeFixture(t)

	svcPool, err := pgxpool.New(t.Context(), f.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	defer svcPool.Close()
	svc := api.NewService(db.New(svcPool), nil, nil, deploy.NewLogHub())

	url, sid := startSession(t, f.authn, svc, f.token)
	svcPool.Close()

	result := callTool(t, url, f.token, sid,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"krill_whoami","arguments":{}}}`)

	if !result.IsError {
		t.Fatalf("want isError once the service's database is gone, got a success result: %+v", result.Content)
	}
	if len(result.Content) != 1 {
		t.Fatalf("want exactly one content block, got %d: %+v", len(result.Content), result.Content)
	}
	// Exact equality again, for the same reason as above. The request id is
	// empty here because this harness has no chi RequestID middleware in front
	// of the handler; in production it is populated, and either way it is the
	// only variable part of the text a caller ever sees.
	const want = "internal error, see server logs (request_id=)"
	if got := result.Content[0].Text; got != want {
		t.Fatalf("internal error text leaked detail beyond the generic message:\ngot  %q\nwant %q", got, want)
	}
}

// TestProtocolSmokeRejectsMissingToken proves the /mcp surface actually
// requires the bearer token this package's own auth wrapper checks for, so
// TestProtocolSmoke's "valid token" premise is exercised, not assumed.
func TestProtocolSmokeRejectsMissingToken(t *testing.T) {
	f := newSmokeFixture(t)
	handler := withBearerAuth(f.authn, mcpsrv.New(f.svc).Handler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"v0"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 with no bearer token, got %d", resp.StatusCode)
	}
}
