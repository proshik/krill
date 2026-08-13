package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
)

// TestSplitAppRef pins the one genuinely ambiguous piece of the REST adapter:
// how the wildcard remainder of /api/v1/apps/* splits into an application
// reference (numeric id or "project/environment/app" path, itself possibly
// containing slashes) and an optional trailing verb.
func TestSplitAppRef(t *testing.T) {
	cases := []struct{ in, wantRef, wantVerb string }{
		// The brief's five baseline cases.
		{"17", "17", ""},
		{"17/logs", "17", "logs"},
		{"acme/production/bot", "acme/production/bot", ""},
		{"acme/production/bot/deploy", "acme/production/bot", "deploy"},
		{"acme/production/bot/env", "acme/production/bot", "env"},

		// Every other recognized verb, once, so a copy/paste typo in the verb
		// set shows up here rather than only in a handler dispatch test.
		{"acme/production/bot/rebuild", "acme/production/bot", "rebuild"},
		{"acme/production/bot/reload", "acme/production/bot", "reload"},
		{"acme/production/bot/stop", "acme/production/bot", "stop"},
		{"acme/production/bot/deployments", "acme/production/bot", "deployments"},

		// A bare verb-looking single segment: is "deploy" an app named
		// "deploy", or a verb with no app? Decision: a verb only ever
		// qualifies a PRECEDING reference. With nothing before it, there is
		// nothing to peel off, so the whole segment is the reference. This
		// also keeps the function total instead of returning an empty ref
		// the caller would have to special-case.
		{"deploy", "deploy", ""},
		{"logs", "logs", ""},

		// A numeric id is never mistaken for a verb even though "17" is a
		// single segment too — same rule as above, just spelled with digits.
		{"17/env", "17", "env"},

		// A trailing slash must not change the result: ".../17/logs/" is the
		// same request as ".../17/logs".
		{"17/logs/", "17", "logs"},
		{"acme/production/bot/deploy/", "acme/production/bot", "deploy"},

		// An empty remainder (e.g. a request to "/api/v1/apps/" itself,
		// which chi's wildcard would capture as "") must not panic and must
		// not fabricate a ref out of nothing.
		{"", "", ""},

		// A trailing-slash-only remainder collapses to empty the same way.
		{"/", "", ""},

		// A non-verb last segment leaves the whole thing as the reference,
		// even when it looks deploy-adjacent.
		{"acme/production/deployer", "acme/production/deployer", ""},

		// The exact collision this function's doc comment calls out: a
		// 3-segment reference whose LAST segment happens to spell a verb
		// (an app literally named "deploy" inside acme/production) is
		// indistinguishable, syntactically, from a valid 4-segment
		// "ref/verb" pair — so it is read as one. See
		// TestAPIAppRouterVerbCollisionWithAppName for how the two HTTP
		// methods diverge on this exact string at the handler layer.
		{"acme/production/deploy", "acme/production", "deploy"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			ref, verb := server.SplitAppRef(tc.in)
			if ref != tc.wantRef || verb != tc.wantVerb {
				t.Fatalf("SplitAppRef(%q) = (%q,%q), want (%q,%q)", tc.in, ref, verb, tc.wantRef, tc.wantVerb)
			}
		})
	}
}

// twoOrgFixture holds the ids used by TestAPIRoutesRejectForeignOrgToken: an
// application (and one deployment under it) living in org A, plus a token
// scoped to org B — the app must be unreachable through every route.
type twoOrgFixture struct {
	appID        string
	deployID     string
	foreignToken string
}

// seedTwoOrgs creates org A (with a project/environment/app and one
// deployment) and org B (empty), and issues a write-level API token for B's
// owner. The token is deliberately write-level, not read-level: every write
// route (deploy/rebuild/reload/stop/env) gates on write capability BEFORE
// resolving the app (see internal/api's requireWrite-before-resolveApp
// ordering, which exists so a read-only token can't distinguish "forbidden"
// from "not found" and enumerate apps that way). A read-level foreign token
// would 403 on those routes for lacking write rights, not 404 for the app
// being in a different org — which would look like tenancy is enforced
// without actually exercising the check this test exists to catch.
func seedTwoOrgs(t *testing.T, q *db.Queries, orgSvc *org.Service) twoOrgFixture {
	t.Helper()
	ctx := context.Background()

	ownerA := mkUser(t, q, "api-idor-a@k.local")
	orgA, err := orgSvc.CreateOrg(ctx, ownerA, "APIIdorOrgA")
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	projA, err := orgSvc.CreateProject(ctx, orgA.ID, "proj-a", "")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}
	envA, err := orgSvc.CreateEnvironment(ctx, projA.ID, "prod")
	if err != nil {
		t.Fatalf("create environment A: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: envA.ID, Name: "bot", Image: "nginx", Tag: "alpine",
		Domain: "api-idor-bot.example.test", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application A: %v", err)
	}
	dep, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("create deployment A: %v", err)
	}

	ownerB := mkUser(t, q, "api-idor-b@k.local")
	orgB, err := orgSvc.CreateOrg(ctx, ownerB, "APIIdorOrgB")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	token := issueAPIToken(t, q, ownerB, orgB.ID, api.LevelWrite)

	return twoOrgFixture{
		appID:        i64(app.ID),
		deployID:     i64(dep.ID),
		foreignToken: token,
	}
}

// TestAPIRoutesRejectForeignOrgToken walks every one of the twelve REST
// routes with a token scoped to a different org than the one holding
// fx.appID/fx.deployID, and asserts 404 on each. This is the test that would
// catch a handler that forgot to pass the identity through to internal/api,
// or that tried to resolve the app itself instead of letting the service do
// it — either mistake would leak org-A's app/deployment existence (or
// contents) to an org-B token. whoami and the plain apps list are
// deliberately excluded: they carry no app reference to leak through, so
// there is nothing cross-tenant to assert here.
func TestAPIRoutesRejectForeignOrgToken(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedTwoOrgs(t, q, orgSvc)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/apps/" + fx.appID},
		{http.MethodGet, "/api/v1/apps/" + fx.appID + "/logs"},
		{http.MethodGet, "/api/v1/apps/" + fx.appID + "/env"},
		{http.MethodGet, "/api/v1/apps/" + fx.appID + "/deployments"},
		{http.MethodGet, "/api/v1/deployments/" + fx.deployID},
		{http.MethodPost, "/api/v1/apps/" + fx.appID + "/deploy"},
		{http.MethodPost, "/api/v1/apps/" + fx.appID + "/rebuild"},
		{http.MethodPost, "/api/v1/apps/" + fx.appID + "/reload"},
		{http.MethodPost, "/api/v1/apps/" + fx.appID + "/stop"},
		{http.MethodPost, "/api/v1/apps/" + fx.appID + "/env"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+fx.foreignToken)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("want 404 for cross-org access, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// apiSuccessFixture holds the ids used by the success-path tests below: one
// organization, one write-level owner token, and one application reachable
// both by numeric id and by its "project/environment/app" path form. It is
// the same-org counterpart to twoOrgFixture — where that fixture proves
// cross-org access is rejected, this one proves same-org access actually
// works and returns the right data, which twoOrgFixture's always-404
// assertions cannot exercise.
type apiSuccessFixture struct {
	orgID   int64
	orgName string
	appID   int64
	path    string // "project/environment/app" form of the same app
	token   string
}

func seedAPISuccessFixture(t *testing.T, q *db.Queries, orgSvc *org.Service) apiSuccessFixture {
	t.Helper()
	ctx := context.Background()

	owner := mkUser(t, q, "api-success@k.local")
	o, err := orgSvc.CreateOrg(ctx, owner, "APISuccessOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(ctx, o.ID, "proj", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	env, err := orgSvc.CreateEnvironment(ctx, proj.ID, "prod")
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: env.ID, Name: "bot", Image: "nginx", Tag: "alpine",
		Domain: "api-success-bot.example.test", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	// Write-level, owner role: exercises both read and write routes with one
	// token, and CanWrite (Level==write && Role>=admin) is true unconditionally.
	token := issueAPIToken(t, q, owner, o.ID, api.LevelWrite)

	return apiSuccessFixture{
		orgID:   o.ID,
		orgName: o.Name,
		appID:   app.ID,
		path:    proj.Name + "/" + env.Name + "/" + app.Name,
		token:   token,
	}
}

// assertAPIErrorCode decodes an error-shaped response body (writeAPIError's
// {"code","message","request_id"}) and checks its "code" field.
func assertAPIErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (body: %s)", err, rec.Body.String())
	}
	if body.Code != want {
		t.Fatalf("code = %q, want %q (body: %s)", body.Code, want, rec.Body.String())
	}
}

// TestAPIWhoamiReturnsCallerIdentity is the adapter's simplest success path:
// a read that returns a body, and the body's fields actually reflect the
// caller resolved from the token — not just a 200 with an empty object (the
// stub apiWhoami this handler replaced would have passed a weaker version of
// this test).
func TestAPIWhoamiReturnsCallerIdentity(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+fx.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got api.WhoamiResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if got.OrgID != fx.orgID {
		t.Errorf("OrgID = %d, want %d", got.OrgID, fx.orgID)
	}
	if got.OrgName != fx.orgName {
		t.Errorf("OrgName = %q, want %q", got.OrgName, fx.orgName)
	}
	if got.Level != string(api.LevelWrite) {
		t.Errorf("Level = %q, want %q", got.Level, api.LevelWrite)
	}
	if !got.CanWrite {
		t.Error("CanWrite = false, want true for a write-level owner token")
	}
}

// TestAPIListAppsReturnsSeededApp covers the other read shape: a list route,
// and that the seeded app is actually present in it (not just a 200 with an
// empty array, which a handler that never called through to the service
// would also produce).
func TestAPIListAppsReturnsSeededApp(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+fx.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var apps []api.AppSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &apps); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	var found *api.AppSummary
	for i := range apps {
		if apps[i].ID == fx.appID {
			found = &apps[i]
		}
	}
	if found == nil {
		t.Fatalf("seeded app %d not present in %+v", fx.appID, apps)
	}
	if found.Path != fx.path {
		t.Errorf("Path = %q, want %q", found.Path, fx.path)
	}
}

// TestAPIAppStatusResolvesNumericAndPathRef is the one that proves
// SplitAppRef's wildcard split works end-to-end over real HTTP, not only in
// the unit table: the same application, fetched by numeric id and by its
// "project/environment/app" path, must both 200 and resolve to the same app.
func TestAPIAppStatusResolvesNumericAndPathRef(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)

	refs := []string{i64(fx.appID), fx.path}
	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+ref, nil)
			req.Header.Set("Authorization", "Bearer "+fx.token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var status api.AppStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
			}
			if status.ID != fx.appID {
				t.Errorf("ID = %d, want %d", status.ID, fx.appID)
			}
		})
	}
}

// TestAPIAppLogsQueryParams proves tail and level actually reach
// internal/api.AppLogs rather than being parsed and discarded. A malformed
// tail is caught by the handler's own defensive strconv.Atoi before ever
// calling the service; an unrecognized level, by contrast, is only rejected
// because internal/api.parseLevelFilter validates it — the handler has no
// opinion of its own on what a valid level is, so a 400 there is only
// possible if the query parameter genuinely made it through.
func TestAPIAppLogsQueryParams(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)

	call := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+i64(fx.appID)+"/logs"+query, nil)
		req.Header.Set("Authorization", "Bearer "+fx.token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("valid tail and level succeed", func(t *testing.T) {
		rec := call("?tail=5&level=info")
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var lines []api.LogLine
		if err := json.Unmarshal(rec.Body.Bytes(), &lines); err != nil {
			t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
		}
	})

	t.Run("non-numeric tail is invalid at the handler", func(t *testing.T) {
		rec := call("?tail=notanumber")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
		}
		assertAPIErrorCode(t, rec, "invalid")
	})

	t.Run("unrecognized level is rejected by the service, proving it reached AppLogs", func(t *testing.T) {
		rec := call("?level=not-a-real-level")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
		}
		assertAPIErrorCode(t, rec, "invalid")
	})
}

// TestAPISetEnvPersistsKeyValue proves a POST body actually decodes and
// reaches the store: after the call, the application's stored env_text
// contains the key/value pair sent in the JSON body — not just a 200.
func TestAPISetEnvPersistsKeyValue(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+i64(fx.appID)+"/env", strings.NewReader(`{"key":"FOO","value":"bar"}`))
	req.Header.Set("Authorization", "Bearer "+fx.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	app, err := q.GetApplication(context.Background(), fx.appID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if !strings.Contains(app.EnvText, "FOO=bar") {
		t.Fatalf("env_text = %q, want it to contain %q", app.EnvText, "FOO=bar")
	}
}

// TestAPISetEnvMalformedBodyIsInvalid proves malformed JSON is rejected as
// api.Invalid (400, code "invalid") rather than reaching the service at all
// or blowing up into a 500.
func TestAPISetEnvMalformedBodyIsInvalid(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+i64(fx.appID)+"/env", strings.NewReader(`{"key":`))
	req.Header.Set("Authorization", "Bearer "+fx.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertAPIErrorCode(t, rec, "invalid")
}

// TestAPIDeployReturnsDeploymentID proves the write routes' async contract:
// a deploy call returns 200 with a real, non-zero deployment id the caller
// can poll, not just an empty acknowledgement.
func TestAPIDeployReturnsDeploymentID(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+i64(fx.appID)+"/deploy", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+fx.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res api.DeployAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if res.DeploymentID == 0 {
		t.Error("DeploymentID = 0, want non-zero")
	}
	if res.Status != "running" {
		t.Errorf("Status = %q, want %q", res.Status, "running")
	}
}

// TestAPIAppRouterVerbCollisionWithAppName pins the accepted ambiguity
// documented on SplitAppRef: a 3-segment app reference whose last segment
// happens to spell a verb is read as (ref, verb), and the two HTTP methods
// then diverge on the exact same string — a GET 404s in the router before
// ever reaching internal/api, while a POST reaches resolveApp and 400s
// there on the truncated ref. Both are decisions, not accidents; this test
// is what holds them there instead of letting a future refactor silently
// pick a third behavior.
func TestAPIAppRouterVerbCollisionWithAppName(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	// Any write-level token proves the point: the request never resolves to
	// a real application on either path (GET 404s in the router itself,
	// POST 400s on an invalid ref shape), so no app named "deploy" needs to
	// actually exist in this org for the collision to be exercised.
	fx := seedAPISuccessFixture(t, q, orgSvc)

	t.Run("GET 404s in the router, never reaches resolveApp", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/acme/production/deploy", nil)
		req.Header.Set("Authorization", "Bearer "+fx.token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("POST reaches resolveApp and 400s on the truncated ref", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/acme/production/deploy", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+fx.token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
		}
		assertAPIErrorCode(t, rec, "invalid")
	})
}
