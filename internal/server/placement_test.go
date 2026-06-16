package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestSavePlacement(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID, _, orgID := rpFixture(t, h, q, orgSvc, "pl-save@k.local")

	// invalid mode -> err flash
	if rec := postForm(t, h, base+"/placement", cookie, url.Values{"placement_mode": {"bogus"}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("invalid mode want 303+err, got %d", rec.Code)
	}
	// any -> persisted, no nodes
	if rec := postForm(t, h, base+"/placement", cookie, url.Values{"placement_mode": {"any"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("any want 303, got %d", rec.Code)
	}
	app, _ := q.GetApplication(ctx, appID)
	if app.PlacementMode != "any" || app.PlacementNodes != "" {
		t.Fatalf("placement = %q / %q", app.PlacementMode, app.PlacementNodes)
	}
	// (node-ID validation requires a live engine; covered by the live 2-node test.)

	// member is blocked
	memberID := mkUser(t, q, "pl-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "pl-member@k.local")
	if rec := postForm(t, h, base+"/placement", mc, url.Values{"placement_mode": {"any"}}); rec.Code != http.StatusForbidden {
		t.Errorf("member placement want 403, got %d", rec.Code)
	}
}

func TestSetDBNode(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "dbnode@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "OrgDBN")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "prod")
	pg, err := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID: e.ID, Name: "db", AppName: "krill-postgres-dbn",
		DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: "pw", Image: "postgres:17",
	})
	if err != nil {
		t.Fatalf("create postgres: %v", err)
	}
	cookie := loginAs(t, q, "dbnode@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases/postgres/" + i64(pg.ID)

	// node="" (control-plane) -> persisted empty
	if rec := postForm(t, h, base+"/node", cookie, url.Values{"node_hostname": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("manager node want 303, got %d body %s", rec.Code, rec.Body.String())
	}
	got, _ := q.GetPostgres(ctx, pg.ID)
	if got.NodeHostname != "" {
		t.Errorf("node_hostname want empty, got %q", got.NodeHostname)
	}
	// (hostname validation requires a live engine; covered by the live 2-node test.)
}
