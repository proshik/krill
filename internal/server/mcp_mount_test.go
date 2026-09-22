package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// newMCPServers builds two routers over ONE database: one with AgentAPIEnabled set,
// one without. internal/mcpsrv's own tests drive the MCP handler through a
// stand-in auth wrapper (it cannot import internal/server without a cycle);
// these exercise the production wiring instead — the real chi mount, the real
// RequireAPIToken, and the real config gate.
func newMCPServers(t *testing.T) (enabled, disabled http.Handler, q *db.Queries, orgSvc *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q = db.New(pool)
	orgSvc = org.NewService(q)
	hub := deploy.NewLogHub()
	build := func(mcpEnabled bool) http.Handler {
		cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", AgentAPIEnabled: mcpEnabled}
		dbSvc := dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net")
		srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbSvc)
		// Engine/deployer nil: krill_whoami, the only tool called here, touches
		// nothing but the database.
		srv.SetAPI(api.NewAuthenticator(q, orgSvc), api.NewService(q, nil, nil))
		return srv.Router()
	}
	return build(true), build(false), q, orgSvc
}

// mcpRequest builds one streamable-HTTP POST to path. An empty token omits the
// Authorization header entirely (the unauthenticated case); an empty sid omits
// the session header (the initialize case).
func mcpRequest(path, body, token, sid string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	return req
}

const mcpInitializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mount-test","version":"v0"}}}`

// mcpRPCEnvelope is the minimal JSON-RPC 2.0 response shape these tests read.
type mcpRPCEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func decodeMCPEnvelope(t *testing.T, rec *httptest.ResponseRecorder) mcpRPCEnvelope {
	t.Helper()
	var env mcpRPCEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode JSON-RPC envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Error != nil {
		t.Fatalf("JSON-RPC error: %s", env.Error.Message)
	}
	return env
}

// TestMCPRejectsMissingToken proves /mcp really sits behind RequireAPIToken:
// without a bearer token the request never reaches the MCP handler.
func TestMCPRejectsMissingToken(t *testing.T) {
	enabled, _, _, _ := newMCPServers(t)
	rec := httptest.NewRecorder()
	enabled.ServeHTTP(rec, mcpRequest("/mcp", mcpInitializeBody, "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a bearer token, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("want a WWW-Authenticate challenge")
	}
}

// TestMCPDisabledIsNotMounted proves the KRILL_AGENT_API_ENABLED gate is real: with
// the flag off the route does not exist at all, even for a valid token (404,
// not 401 — an unmounted path, not a rejected credential).
func TestMCPDisabledIsNotMounted(t *testing.T) {
	_, disabled, q, orgSvc := newMCPServers(t)
	uid := mkUser(t, q, "mcp-off@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), uid, "MCPOffOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token := issueAPIToken(t, q, uid, o.ID, api.LevelRead)

	rec := httptest.NewRecorder()
	disabled.ServeHTTP(rec, mcpRequest("/mcp", mcpInitializeBody, token, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 with AgentAPIEnabled=false, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestMCPIdentityReachesToolsCall is the load-bearing test for the production
// mount: it drives initialize → tools/call through the real router, and asserts
// the tools/call — a LATER request in the session, not the one that created it —
// comes back with THIS test's own seeded org id. The identity a tool sees can
// only have arrived via RequireAPIToken → api.WithIdentity → the request context
// → api.IdentityFrom inside the tool handler, so a passing assertion here is
// direct evidence that context propagation survives past initialize with a
// single shared *mcp.Server (the premise the whole adapter design rests on).
//
// It also sends the follow-up to /mcp/sub rather than /mcp, proving the two
// mount points share one handler (and therefore one session table): a second,
// separately built handler would not recognize this session id.
func TestMCPIdentityReachesToolsCall(t *testing.T) {
	enabled, _, q, orgSvc := newMCPServers(t)
	uid := mkUser(t, q, "mcp-mount@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), uid, "MCPMountOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token := issueAPIToken(t, q, uid, o.ID, api.LevelRead)

	rec := httptest.NewRecorder()
	enabled.ServeHTTP(rec, mcpRequest("/mcp", mcpInitializeBody, token, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	decodeMCPEnvelope(t, rec)
	sid := rec.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize: no Mcp-Session-Id returned")
	}

	rec = httptest.NewRecorder()
	enabled.ServeHTTP(rec, mcpRequest("/mcp/sub",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"krill_whoami","arguments":{}}}`, token, sid))
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	env := decodeMCPEnvelope(t, rec)

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
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
	if who.OrgID != o.ID {
		t.Fatalf("whoami org id = %d, want %d — the caller's identity did not reach the tools/call handler", who.OrgID, o.ID)
	}
	if who.UserID != uid {
		t.Fatalf("whoami user id = %d, want %d", who.UserID, uid)
	}
	if who.CanWrite {
		t.Fatal("whoami: read-level token — CanWrite should be false")
	}
}

// mcpInit opens an MCP session through the production router and returns its
// session id.
func mcpInit(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mcpRequest("/mcp", mcpInitializeBody, token, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	decodeMCPEnvelope(t, rec)
	sid := rec.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize: no Mcp-Session-Id returned")
	}
	return sid
}

// mcpToolResult is the wire shape of a tools/call result.
type mcpToolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// mcpCall sends one tools/call in session sid and returns the HTTP status and,
// for a 200, the decoded tool result.
func mcpCall(t *testing.T, h http.Handler, token, sid, body string) (int, mcpToolResult) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mcpRequest("/mcp", body, token, sid))
	var result mcpToolResult
	if rec.Code != http.StatusOK {
		return rec.Code, result
	}
	env := decodeMCPEnvelope(t, rec)
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	return rec.Code, result
}

func mcpWhoami(t *testing.T, res mcpToolResult) api.WhoamiResult {
	t.Helper()
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("krill_whoami failed: %+v", res.Content)
	}
	var who api.WhoamiResult
	if err := json.Unmarshal([]byte(res.Content[0].Text), &who); err != nil {
		t.Fatalf("decode WhoamiResult: %v (text=%q)", err, res.Content[0].Text)
	}
	return who
}

const (
	mcpWhoamiBody = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"krill_whoami","arguments":{}}}`
	mcpSetEnvFmt  = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"krill_set_env","arguments":{"app":"%d","key":"%s","value":"1"}}}`
)

// seedMCPApp creates an image application in a fresh project/environment of
// orgID and returns its id.
func seedMCPApp(t *testing.T, q *db.Queries, orgSvc *org.Service, orgID int64) int64 {
	t.Helper()
	ctx := context.Background()
	p, err := orgSvc.CreateProject(ctx, orgID, "Proj", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	e, err := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	return a.ID
}

// TestMCPSessionSeesDemotion: a session opened while the token's owner could
// write must lose write access the moment the owner is demoted — every
// request is authenticated afresh, and a tool must act on THAT identity, not
// on the one captured when the session was initialized.
func TestMCPSessionSeesDemotion(t *testing.T) {
	h, _, q, orgSvc := newMCPServers(t)
	ctx := context.Background()
	uid := mkUser(t, q, "mcp-demote@k.local")
	o, err := orgSvc.CreateOrg(ctx, uid, "MCPDemoteOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	appID := seedMCPApp(t, q, orgSvc, o.ID)
	token := issueAPIToken(t, q, uid, o.ID, api.LevelWrite)
	sid := mcpInit(t, h, token)

	// Sanity: the session can write before the demotion, so the refusal
	// below is caused by the demotion and nothing else.
	if code, res := mcpCall(t, h, token, sid, fmt.Sprintf(mcpSetEnvFmt, appID, "BEFORE")); code != http.StatusOK || res.IsError {
		t.Fatalf("set_env before demotion: status %d, result %+v", code, res)
	}

	m, err := q.GetMembership(ctx, db.GetMembershipParams{OrganizationID: o.ID, UserID: uid})
	if err != nil {
		t.Fatalf("get membership: %v", err)
	}
	if err := q.UpdateMemberRole(ctx, db.UpdateMemberRoleParams{ID: m.ID, Role: "member"}); err != nil {
		t.Fatalf("demote: %v", err)
	}

	code, res := mcpCall(t, h, token, sid, mcpWhoamiBody)
	if code != http.StatusOK {
		t.Fatalf("whoami after demotion: want 200, got %d", code)
	}
	if who := mcpWhoami(t, res); who.CanWrite || who.Role != "member" {
		t.Fatalf("whoami after demotion: role=%q can_write=%v, want member/false", who.Role, who.CanWrite)
	}

	code, res = mcpCall(t, h, token, sid, fmt.Sprintf(mcpSetEnvFmt, appID, "AFTER"))
	if code != http.StatusOK {
		t.Fatalf("set_env after demotion: want 200, got %d", code)
	}
	if !res.IsError || len(res.Content) == 0 || !strings.HasPrefix(res.Content[0].Text, "forbidden:") {
		t.Fatalf("set_env after demotion: want a forbidden tool error, got %+v", res)
	}
	app, err := q.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if strings.Contains(app.EnvText, "AFTER=") {
		t.Fatalf("demoted member still wrote env_text: %q", app.EnvText)
	}
}

// TestMCPSessionRejectsAnotherToken: a request carrying a leaked
// Mcp-Session-Id with a different, perfectly valid token must never run as
// the session's owner.
func TestMCPSessionRejectsAnotherToken(t *testing.T) {
	h, _, q, orgSvc := newMCPServers(t)
	ctx := context.Background()
	uidA := mkUser(t, q, "mcp-owner-a@k.local")
	orgA, err := orgSvc.CreateOrg(ctx, uidA, "MCPOrgA")
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	appA := seedMCPApp(t, q, orgSvc, orgA.ID)
	tokenA := issueAPIToken(t, q, uidA, orgA.ID, api.LevelWrite)

	uidB := mkUser(t, q, "mcp-owner-b@k.local")
	orgB, err := orgSvc.CreateOrg(ctx, uidB, "MCPOrgB")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	tokenB := issueAPIToken(t, q, uidB, orgB.ID, api.LevelRead)

	sid := mcpInit(t, h, tokenA)

	code, res := mcpCall(t, h, tokenB, sid, mcpWhoamiBody)
	if code == http.StatusOK {
		if who := mcpWhoami(t, res); who.UserID != uidB || who.OrgID != orgB.ID {
			t.Fatalf("token B in A's session ran as user %d org %d, want B (%d/%d) or a refusal", who.UserID, who.OrgID, uidB, orgB.ID)
		}
	} else if code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("token B in A's session: want 403/404 or B's own identity, got %d", code)
	}

	mcpCall(t, h, tokenB, sid, fmt.Sprintf(mcpSetEnvFmt, appA, "HIJACK"))
	app, err := q.GetApplication(ctx, appA)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if strings.Contains(app.EnvText, "HIJACK=") {
		t.Fatalf("token B wrote A's env_text through A's session: %q", app.EnvText)
	}

	// The owner's own session keeps working after the refused attempt.
	code, res = mcpCall(t, h, tokenA, sid, mcpWhoamiBody)
	if code != http.StatusOK {
		t.Fatalf("owner's whoami after the attempt: want 200, got %d", code)
	}
	if who := mcpWhoami(t, res); who.UserID != uidA {
		t.Fatalf("owner's whoami: user %d, want %d", who.UserID, uidA)
	}
}
