package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
)

// TestMemberCannotMutateApp locks in the Critical fix that all app POST mutations
// live under RequireRole(admin). A plain "member" of the org must be blocked with
// 403 BEFORE the handler runs (so no deployer/engine is needed for these paths),
// while an owner passes the RBAC gate (and is therefore not 403 on the same paths).
func TestMemberCannotMutateApp(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	// Owner builds the org → project → environment → app chain.
	ownerID := mkUser(t, q, "rbac-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "rbac.example.com", Port: 80, Env: map[string]string{}, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}

	// Put the app into a non-default status so a blocked /stop (which would set
	// "idle") is observably a no-op. Apps default to "idle".
	if err := q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: a.ID, Status: deploy.StatusRunning}); err != nil {
		t.Fatalf("seed running status: %v", err)
	}

	// A SECOND user joined to the same org as a plain member.
	memberID := mkUser(t, q, "rbac-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) +
		"/environments/" + i64(e.ID) + "/apps/" + i64(a.ID)

	// The mutating POST paths that must be admin-gated.
	mutations := []string{"/deploy", "/stop", "/env", "/advanced"}

	// As a member: every mutation must be 403, blocked by RequireRole before the
	// handler — so the nil deployer/engine in the test server is never reached.
	memberCookie := loginAs(t, q, "rbac-member@k.local")
	for _, suffix := range mutations {
		rec := postForm(t, h, base+suffix, memberCookie, url.Values{})
		if rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s want 403, got %d", suffix, rec.Code)
		}
		// The member must not have stopped/idled the app via /stop.
		if suffix == "/stop" {
			got, gerr := q.GetApplication(ctx, a.ID)
			if gerr != nil {
				t.Fatalf("GetApplication after member /stop: %v", gerr)
			}
			if got.Status == deploy.StatusIdle {
				t.Errorf("member /stop must not mark app idle, status=%q", got.Status)
			}
		}
	}

	// As the owner: the RBAC gate must NOT block these paths. We only assert the
	// owner is not 403; the actual outcome (303 success or handler-level result)
	// depends on the handler. /stop and /env touch neither deployer nor engine
	// (engine is nil), so they succeed with 303; we restrict the owner not-403
	// assertion to those to avoid the nil-deployer panic on /deploy.
	ownerCookie := loginAs(t, q, "rbac-owner@k.local")
	for _, suffix := range []string{"/stop", "/env", "/advanced"} {
		form := url.Values{}
		if suffix == "/advanced" {
			// saveAdvanced requires a valid minimal form to reach the success path.
			form = url.Values{"replicas": {"1"}, "restart_condition": {"any"}}
		}
		rec := postForm(t, h, base+suffix, ownerCookie, form)
		if rec.Code == http.StatusForbidden {
			t.Errorf("owner POST %s must NOT be 403 (RBAC should allow), got 403", suffix)
		}
	}
}

// TestSaveEnvPersists verifies the saveEnv success path: an owner POST persists
// the parsed env map via UpdateApplicationEnv (no engine/deployer involved).
func TestSaveEnvPersists(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "saveenv.example.com")

	rec := postForm(t, h, base+"/env", cookie, url.Values{
		"env": {"FOO=bar\nBAZ=qux"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saveEnv want 303, got %d body: %s", rec.Code, rec.Body.String())
	}

	a, err := q.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if a.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] want %q, got %q", "bar", a.Env["FOO"])
	}
	if a.Env["BAZ"] != "qux" {
		t.Errorf("Env[BAZ] want %q, got %q", "qux", a.Env["BAZ"])
	}
}

// TestStopAppMarksIdle verifies stopApp's nil-engine path: with no engine wired
// (as in the test server) it skips the scale-to-zero and simply marks the app
// idle, then redirects with 303.
func TestStopAppMarksIdle(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "stopapp.example.com")

	rec := postForm(t, h, base+"/stop", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("stopApp want 303, got %d body: %s", rec.Code, rec.Body.String())
	}

	a, err := q.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if a.Status != deploy.StatusIdle {
		t.Errorf("Status want %q, got %q", deploy.StatusIdle, a.Status)
	}
}
