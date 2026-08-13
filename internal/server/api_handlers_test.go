package server_test

import (
	"context"
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
