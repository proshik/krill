package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// createRegistryForm POSTs a registry create form and returns the recorder.
func createRegistryForm(t *testing.T, h http.Handler, cookie *http.Cookie, orgID int64, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	target := "/orgs/" + i64(orgID) + "/registries"
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateRegistrySucceeds(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "owner@k.local")

	form := url.Values{
		"name":         {"primary"},
		"registry_url": {"registry.example.com"},
		"username":     {"robot"},
		"password":     {"secret"},
	}
	rec := createRegistryForm(t, h, cookie, o.ID, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create registry want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	regs, err := q.ListRegistriesByOrg(ctx, o.ID)
	if err != nil {
		t.Fatalf("list registries: %v", err)
	}
	if len(regs) != 1 || regs[0].Name != "primary" || regs[0].RegistryUrl != "registry.example.com" {
		t.Fatalf("expected 1 registry 'primary', got %+v", regs)
	}
}

func TestCreateRegistryDuplicateName400(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "owner@k.local")

	form := url.Values{
		"name":         {"primary"},
		"registry_url": {"registry.example.com"},
		"username":     {"robot"},
		"password":     {"secret"},
	}
	if rec := createRegistryForm(t, h, cookie, o.ID, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("first create want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Same name again → 400.
	rec := createRegistryForm(t, h, cookie, o.ID, form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name want 400, got %d", rec.Code)
	}

	regs, _ := q.ListRegistriesByOrg(ctx, o.ID)
	if len(regs) != 1 {
		t.Fatalf("expected 1 registry after duplicate, got %d", len(regs))
	}
}

func TestMemberCannotCreateRegistry(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memberID := mkUser(t, q, "member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie := loginAs(t, q, "member@k.local")

	form := url.Values{
		"name":         {"primary"},
		"registry_url": {"registry.example.com"},
		"username":     {"robot"},
		"password":     {"secret"},
	}
	rec := createRegistryForm(t, h, cookie, o.ID, form)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member create registry want 403, got %d", rec.Code)
	}

	regs, _ := q.ListRegistriesByOrg(ctx, o.ID)
	if len(regs) != 0 {
		t.Fatalf("expected no registries, got %d", len(regs))
	}
}

func TestDeleteUnreferencedRegistry(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "owner@k.local")

	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{
		OrganizationID: o.ID,
		Name:           "primary",
		RegistryUrl:    "registry.example.com",
		Username:       "robot",
		Password:       "secret",
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}

	target := "/orgs/" + i64(o.ID) + "/registries/" + i64(reg.ID) + "/delete"
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete registry want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	if _, err := q.GetRegistry(ctx, reg.ID); err == nil {
		t.Fatalf("registry should be deleted")
	}
}

func TestCreateAppWithRegistrySetsRegistryID(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{
		OrganizationID: o.ID, Name: "primary", RegistryUrl: "registry.example.com",
		Username: "robot", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	cookie := loginAs(t, q, "owner@k.local")

	appsURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps"
	rec := postForm(t, h, appsURL, cookie, url.Values{
		"name":        {"web"},
		"image":       {"nginx"},
		"tag":         {"latest"},
		"port":        {"80"},
		"registry_id": {i64(reg.ID)},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("createApp want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	apps, _ := q.ListApplicationsByEnvironment(ctx, e.ID)
	if len(apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(apps))
	}
	a, err := q.GetApplication(ctx, apps[0].ID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if a.RegistryID == nil || *a.RegistryID != reg.ID {
		t.Fatalf("expected RegistryID=%d, got %v", reg.ID, a.RegistryID)
	}
}

func TestCreateAppCrossOrgRegistry400(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	ownerAID := mkUser(t, q, "ownera@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, ownerAID, "OrgA")
	regA, err := q.CreateRegistry(ctx, db.CreateRegistryParams{
		OrganizationID: orgA.ID, Name: "a", RegistryUrl: "registry.example.com",
		Username: "robot", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create registry A: %v", err)
	}

	ownerBID := mkUser(t, q, "ownerb@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, ownerBID, "OrgB")
	pB, _ := orgSvc.CreateProject(ctx, orgB.ID, "ProjB", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "production")
	cookieB := loginAs(t, q, "ownerb@k.local")

	appsURL := "/orgs/" + i64(orgB.ID) + "/projects/" + i64(pB.ID) + "/environments/" + i64(eB.ID) + "/apps"
	rec := postForm(t, h, appsURL, cookieB, url.Values{
		"name":        {"web"},
		"image":       {"nginx"},
		"tag":         {"latest"},
		"port":        {"80"},
		"registry_id": {i64(regA.ID)},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-org registry want 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	apps, _ := q.ListApplicationsByEnvironment(ctx, eB.ID)
	if len(apps) != 0 {
		t.Fatalf("expected no apps created, got %d", len(apps))
	}
}

func TestSetAppRegistrySetsAndClears(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "latest", Domain: "web.k.local", Port: 80,
		Env: map[string]string{}, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{
		OrganizationID: o.ID, Name: "primary", RegistryUrl: "registry.example.com",
		Username: "robot", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	cookie := loginAs(t, q, "owner@k.local")

	regURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID) + "/registry"

	// Set the registry.
	if rec := postForm(t, h, regURL, cookie, url.Values{"registry_id": {i64(reg.ID)}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set registry want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := q.GetApplication(ctx, a.ID)
	if got.RegistryID == nil || *got.RegistryID != reg.ID {
		t.Fatalf("expected RegistryID=%d, got %v", reg.ID, got.RegistryID)
	}

	// Clear the registry (empty registry_id → public).
	if rec := postForm(t, h, regURL, cookie, url.Values{"registry_id": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear registry want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ = q.GetApplication(ctx, a.ID)
	if got.RegistryID != nil {
		t.Fatalf("expected RegistryID=nil after clear, got %v", *got.RegistryID)
	}
}

func TestDeleteRegistryCrossTenant404(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	ownerAID := mkUser(t, q, "ownera@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, ownerAID, "OrgA")
	regA, err := q.CreateRegistry(ctx, db.CreateRegistryParams{
		OrganizationID: orgA.ID, Name: "a", RegistryUrl: "registry.example.com",
		Username: "robot", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create registry A: %v", err)
	}

	ownerBID := mkUser(t, q, "ownerb@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, ownerBID, "OrgB")
	cookieB := loginAs(t, q, "ownerb@k.local")

	// Org-B owner tries to delete org-A's registry via org-B's path.
	target := "/orgs/" + i64(orgB.ID) + "/registries/" + i64(regA.ID) + "/delete"
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.AddCookie(cookieB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete want 404, got %d", rec.Code)
	}
	if _, err := q.GetRegistry(ctx, regA.ID); err != nil {
		t.Fatalf("org-A registry must still exist: %v", err)
	}
}
