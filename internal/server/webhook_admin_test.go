package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// orgIDFromBase parses the org ID from a base URL of the form
// /orgs/{id}/projects/...
func orgIDFromBase(t *testing.T, base string) int64 {
	t.Helper()
	// base looks like /orgs/42/projects/1/environments/1/apps/1
	parts := strings.Split(strings.TrimPrefix(base, "/"), "/")
	// parts[0]="orgs", parts[1]=id
	if len(parts) < 2 || parts[0] != "orgs" {
		t.Fatalf("orgIDFromBase: unexpected base %q", base)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("orgIDFromBase: parse %q: %v", parts[1], err)
	}
	return id
}

func TestAutoDeployAdminGate(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	base, ownerCookie, appID := domainFixture(t, h, q, orgSvc, "ad.example.com")

	// owner enable → 303 + auto_deploy true + secret set
	rec := postForm(t, h, base+"/autodeploy/enable", ownerCookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("enable: want 303, got %d body: %s", rec.Code, rec.Body.String())
	}
	a, _ := q.GetApplication(context.Background(), appID)
	if !a.AutoDeploy || a.WebhookSecret == "" {
		t.Fatalf("after enable: auto_deploy=%v secret_empty=%v", a.AutoDeploy, a.WebhookSecret == "")
	}
	first := a.WebhookSecret

	// regenerate → secret changes, auto_deploy stays true
	rec = postForm(t, h, base+"/autodeploy/regenerate", ownerCookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("regenerate: want 303, got %d body: %s", rec.Code, rec.Body.String())
	}
	a, _ = q.GetApplication(context.Background(), appID)
	if a.WebhookSecret == first || !a.AutoDeploy {
		t.Fatal("regenerate should rotate the secret and keep auto_deploy")
	}
	second := a.WebhookSecret

	// disable → auto_deploy false, secret kept
	rec = postForm(t, h, base+"/autodeploy/disable", ownerCookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: want 303, got %d body: %s", rec.Code, rec.Body.String())
	}
	a, _ = q.GetApplication(context.Background(), appID)
	if a.AutoDeploy || a.WebhookSecret != second {
		t.Fatal("disable should clear the toggle but keep the secret")
	}

	// member cannot mutate
	memberID := mkUser(t, q, "ad-member@k.local")
	if _, err := q.CreateMember(context.Background(), db.CreateMemberParams{
		OrganizationID: orgIDFromBase(t, base), UserID: memberID, Role: "member",
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	memberCookie := loginAs(t, q, "ad-member@k.local")
	for _, suffix := range []string{"/autodeploy/enable", "/autodeploy/disable", "/autodeploy/regenerate"} {
		rec := postForm(t, h, base+suffix, memberCookie, url.Values{})
		if rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s want 403, got %d", suffix, rec.Code)
		}
	}
}
