package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// The registries and git credentials pages list the apps that use each one,
// and only the organization's own apps.
func TestCredentialsUsedBy(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	owner := mkUser(t, q, "usedby-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "UsedBy")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "storefront-api", Image: "nginx", Tag: "1", Domain: "usedby-api.example.com", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	idle, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "idle-app", Image: "nginx", Tag: "1", Domain: "usedby-idle.example.com", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{OrganizationID: o.ID, Name: "ghcr", RegistryUrl: "ghcr.io", Username: "bot", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	gc, err := q.CreateGitCredential(ctx, db.CreateGitCredentialParams{OrganizationID: o.ID, Name: "github", Host: "github.com", Username: "x-access-token", Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: app.ID, RegistryID: &reg.ID}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationGitCredential(ctx, db.SetApplicationGitCredentialParams{ID: app.ID, GitCredentialID: &gc.ID}); err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, q, "usedby-owner@k.local")
	base := "/orgs/" + i64(o.ID)
	appHref := base + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)
	for _, page := range []string{"/registries", "/git-credentials"} {
		rec := getWithCookie(t, h, base+page, cookie)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `href="`+appHref+`?tab=settings#s-source"`) {
			t.Errorf("%s: the app using it is not listed (code %d)", page, rec.Code)
		}
		if strings.Contains(body, "idle-app") {
			t.Errorf("%s: an app that does not use it is listed", page)
		}
	}
	_ = idle

	// Another organization's member never sees these pages.
	mkUser(t, q, "usedby-stranger@k.local")
	stranger := loginAs(t, q, "usedby-stranger@k.local")
	if rec := getWithCookie(t, h, base+"/registries", stranger); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger got %d on another organization's registries", rec.Code)
	}
}

// The members page says which members still have to change their temporary
// password.
func TestMembersShowTemporaryPassword(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	owner := mkUser(t, q, "temp-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "Temp")
	fresh := mkUser(t, q, "temp-fresh@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: fresh, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, q, "temp-owner@k.local")
	path := "/orgs/" + i64(o.ID) + "/members"
	if body := getWithCookie(t, h, path, cookie).Body.String(); strings.Contains(body, "temporary password not changed yet") {
		t.Fatal("a member who changed nothing was shown as owing a password change")
	}
	if err := q.SetUserMustChangePassword(ctx, db.SetUserMustChangePasswordParams{ID: fresh, MustChangePassword: true}); err != nil {
		t.Fatal(err)
	}
	if body := getWithCookie(t, h, path, cookie).Body.String(); !strings.Contains(body, "temporary password not changed yet") {
		t.Fatal("the member who still owes a password change is not marked")
	}
}

// The palette's search only ever returns the organization's own things: an
// app of another organization never appears, not even through the env
// parameter, and instance-wide pages are offered to instance operators only.
func TestOrgSearchTenancy(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	userA := mkUser(t, q, "search-a@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userA, "SearchA")
	pA, _ := orgSvc.CreateProject(ctx, orgA.ID, "alpha", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "production")
	mkApp(t, q, eA.ID, "secret-billing", "search-a.example.com")

	userB := mkUser(t, q, "search-b@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userB, "SearchB")
	pB, _ := orgSvc.CreateProject(ctx, orgB.ID, "beta", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "production")
	mkApp(t, q, eB.ID, "billing-web", "search-b.example.com")
	member := mkUser(t, q, "search-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgB.ID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}

	cookieB := loginAs(t, q, "search-b@k.local")
	baseB := "/orgs/" + i64(orgB.ID) + "/search"
	rec := getWithCookie(t, h, baseB+"?q=billing&env="+i64(eA.ID), cookieB)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "billing-web") {
		t.Fatalf("own app missing from the results (code %d)", rec.Code)
	}
	if strings.Contains(body, "secret-billing") || strings.Contains(body, "alpha") {
		t.Fatal("SECURITY: another organization's app or project in the results")
	}
	// The organization's owner who is not an instance operator gets no
	// instance-wide pages.
	if body := getWithCookie(t, h, baseB+"?q=", cookieB).Body.String(); strings.Contains(body, "/nodes\"") || strings.Contains(body, "/monitoring\"") || strings.Contains(body, "/firewall\"") {
		t.Fatal("instance-wide pages offered to a non-operator")
	}
	// The environment's actions: an admin may create, a member may not.
	if body := getWithCookie(t, h, baseB+"?env="+i64(eB.ID), cookieB).Body.String(); !strings.Contains(body, "?new=app") {
		t.Fatal("the admin is not offered a new app in the environment")
	}
	memberCookie := loginAs(t, q, "search-member@k.local")
	if body := getWithCookie(t, h, baseB+"?env="+i64(eB.ID), memberCookie).Body.String(); strings.Contains(body, "?new=app") || !strings.Contains(body, "billing-web") {
		t.Fatal("a member must see the environment's apps and no create actions")
	}
	// A stranger is refused the search of an organization they do not belong to.
	cookieA := loginAs(t, q, "search-a@k.local")
	if rec := getWithCookie(t, h, baseB+"?q=billing", cookieA); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant search answered %d", rec.Code)
	}
}
