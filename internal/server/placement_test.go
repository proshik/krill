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

	// pin/global with no nodes -> err flash, NOT persisted (would silently behave
	// as "any" otherwise). Runs without a live engine.
	for _, mode := range []string{"pin", "global"} {
		if rec := postForm(t, h, base+"/placement", cookie, url.Values{"placement_mode": {mode}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
			t.Fatalf("%s with no nodes want 303+err, got %d", mode, rec.Code)
		}
		if app, _ := q.GetApplication(ctx, appID); app.PlacementMode != "any" {
			t.Fatalf("%s with no nodes persisted mode=%q, want unchanged 'any'", mode, app.PlacementMode)
		}
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

// TestSetDBInstanceNode exercises setDBInstanceNode — node pinning for a DB
// instance now lives on the org-level /db-servers page (moved off the
// env-level DB detail page in the logical-databases rework).
func TestSetDBInstanceNode(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "dbnode@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "OrgDBN")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-dbn",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	cookie := loginAs(t, q, "dbnode@k.local")
	base := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(inst.ID)

	// node="" (control-plane) -> persisted empty
	if rec := postForm(t, h, base+"/node", cookie, url.Values{"node_hostname": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("manager node want 303, got %d body %s", rec.Code, rec.Body.String())
	}
	got, _ := q.GetDBInstance(ctx, inst.ID)
	if got.NodeHostname != "" {
		t.Errorf("node_hostname want empty, got %q", got.NodeHostname)
	}
	// (hostname validation requires a live engine; covered by the live 2-node test.)
}
