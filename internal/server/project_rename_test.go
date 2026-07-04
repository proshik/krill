package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestRenameProject(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "pr-rename@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "old-name", "")
	cookie := loginAs(t, q, "pr-rename@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID)

	if rec := postForm(t, h, base+"/rename", cookie, url.Values{"name": {"new-name"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("rename: got %d", rec.Code)
	}
	if got, _ := q.GetProject(ctx, p.ID); got.Name != "new-name" {
		t.Fatalf("name = %q, want new-name", got.Name)
	}

	// empty name rejected, unchanged
	postForm(t, h, base+"/rename", cookie, url.Values{"name": {"   "}})
	if got, _ := q.GetProject(ctx, p.ID); got.Name != "new-name" {
		t.Fatalf("empty name should not change, got %q", got.Name)
	}

	// member (non-admin) → 403
	memberID := mkUser(t, q, "pr-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "pr-member@k.local")
	if rec := postForm(t, h, base+"/rename", mc, url.Values{"name": {"hax"}}); rec.Code != http.StatusForbidden {
		t.Errorf("member rename want 403, got %d", rec.Code)
	}
}

func TestRenameProjectCrossTenant(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uidA := mkUser(t, q, "prx-a@k.local")
	oA, _ := orgSvc.CreateOrg(ctx, uidA, "OrgA")
	pA, _ := orgSvc.CreateProject(ctx, oA.ID, "a-proj", "")
	uidB := mkUser(t, q, "prx-b@k.local")
	oB, _ := orgSvc.CreateOrg(ctx, uidB, "OrgB")
	cookieB := loginAs(t, q, "prx-b@k.local")

	target := "/orgs/" + i64(oB.ID) + "/projects/" + i64(pA.ID) + "/rename"
	if rec := postForm(t, h, target, cookieB, url.Values{"name": {"pwned"}}); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant rename want 404, got %d", rec.Code)
	}
	if got, _ := q.GetProject(ctx, pA.ID); got.Name != "a-proj" {
		t.Fatalf("SECURITY: org-A project renamed cross-tenant: %q", got.Name)
	}
}
