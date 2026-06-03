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
