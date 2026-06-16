package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

func TestGitCredentialCRUD(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	_, cookie, _, _, orgID := rpFixture(t, h, q, orgSvc, "gc-crud@k.local")
	gcURL := "/orgs/" + i64(orgID) + "/git-credentials"

	rec := postForm(t, h, gcURL, cookie, url.Values{"name": {"gh"}, "host": {"github.com"}, "username": {"x-access-token"}, "token": {"ghp_x"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create want 303, got %d body %s", rec.Code, rec.Body.String())
	}
	creds, _ := q.ListGitCredentialsByOrg(ctx, orgID)
	if len(creds) != 1 || creds[0].Name != "gh" || creds[0].Host != "github.com" {
		t.Fatalf("credential not created: %+v", creds)
	}
	if secret.Dec(creds[0].Token) != "ghp_x" {
		t.Errorf("token not stored/decryptable: %q", creds[0].Token)
	}

	if rec := postForm(t, h, gcURL, cookie, url.Values{"name": {"x"}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("missing fields want 303+err, got %d", rec.Code)
	}
	if rec := postForm(t, h, gcURL, cookie, url.Values{"name": {"gh"}, "host": {"h"}, "username": {"u"}, "token": {"t"}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("duplicate want 303+err, got %d", rec.Code)
	}

	if rec := postForm(t, h, gcURL+"/"+i64(creds[0].ID)+"/delete", cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete want 303, got %d", rec.Code)
	}
	if n, _ := q.ListGitCredentialsByOrg(ctx, orgID); len(n) != 0 {
		t.Fatalf("credential not deleted: %d", len(n))
	}
}

func TestGitCredentialDeleteGuarded(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, _, orgID := rpFixture(t, h, q, orgSvc, "gc-guard@k.local")
	gcURL := "/orgs/" + i64(orgID) + "/git-credentials"
	postForm(t, h, gcURL, cookie, url.Values{"name": {"gh"}, "host": {"github.com"}, "username": {"u"}, "token": {"t"}})
	creds, _ := q.ListGitCredentialsByOrg(ctx, orgID)
	gcID := creds[0].ID

	if rec := postForm(t, h, base+"/git-credential", cookie, url.Values{"git_credential_id": {i64(gcID)}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("assign want 303, got %d", rec.Code)
	}
	rec := postForm(t, h, gcURL+"/"+i64(gcID)+"/delete", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("guarded delete want 303+err, got %d", rec.Code)
	}
	if _, err := q.GetGitCredential(ctx, gcID); err != nil {
		t.Fatalf("credential should still exist: %v", err)
	}
}

func TestSaveBuild(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID, _, _ := rpFixture(t, h, q, orgSvc, "gc-build@k.local")

	rec := postForm(t, h, base+"/build", cookie, url.Values{"build_args": {"A=1\nB=2"}, "build_secrets": {"S=x"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saveBuild want 303, got %d body %s", rec.Code, rec.Body.String())
	}
	app, _ := q.GetApplication(ctx, appID)
	if app.BuildArgs != "A=1\nB=2" {
		t.Errorf("build_args = %q", app.BuildArgs)
	}
	if secret.Dec(app.BuildSecrets) != "S=x" {
		t.Errorf("build_secrets not stored encrypted/decryptable: %q", app.BuildSecrets)
	}

	if rec := postForm(t, h, base+"/build", cookie, url.Values{"build_args": {"1BAD=x"}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("bad build-arg key want 303+err, got %d", rec.Code)
	}
}

func TestSetAppGitCredentialCrossOrg(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	baseA, cookieA, _, _, _ := rpFixture(t, h, q, orgSvc, "gc-xa@k.local")
	_, cookieB, _, _, orgB := rpFixture(t, h, q, orgSvc, "gc-xb@k.local")
	postForm(t, h, "/orgs/"+i64(orgB)+"/git-credentials", cookieB, url.Values{"name": {"gh"}, "host": {"h"}, "username": {"u"}, "token": {"t"}})
	credsB, _ := q.ListGitCredentialsByOrg(ctx, orgB)

	rec := postForm(t, h, baseA+"/git-credential", cookieA, url.Values{"git_credential_id": {i64(credsB[0].ID)}})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("cross-org credential want 303+err, got %d", rec.Code)
	}
}

func TestGitCredentialDeleteCrossOrg(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	_, cookieA, _, _, orgA := rpFixture(t, h, q, orgSvc, "gc-idora@k.local")
	_, cookieB, _, _, orgB := rpFixture(t, h, q, orgSvc, "gc-idorb@k.local")
	postForm(t, h, "/orgs/"+i64(orgA)+"/git-credentials", cookieA, url.Values{"name": {"gh"}, "host": {"h"}, "username": {"u"}, "token": {"t"}})
	credsA, _ := q.ListGitCredentialsByOrg(ctx, orgA)

	rec := postForm(t, h, "/orgs/"+i64(orgB)+"/git-credentials/"+i64(credsA[0].ID)+"/delete", cookieB, url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-org delete want 404, got %d", rec.Code)
	}
	if _, err := q.GetGitCredential(ctx, credsA[0].ID); err != nil {
		t.Fatalf("org-A credential deleted cross-tenant: %v", err)
	}
}

func TestMemberCannotMutateGitCredential(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, _, _, _, orgID := rpFixture(t, h, q, orgSvc, "gc-gate@k.local")
	memberID := mkUser(t, q, "gc-gate-m@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "gc-gate-m@k.local")
	for _, target := range []string{
		"/orgs/" + i64(orgID) + "/git-credentials",
		base + "/git-credential",
		base + "/build",
	} {
		rec := postForm(t, h, target, mc, url.Values{"name": {"x"}, "host": {"h"}, "username": {"u"}, "token": {"t"}, "build_args": {"A=1"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s want 403, got %d", target, rec.Code)
		}
	}
}
