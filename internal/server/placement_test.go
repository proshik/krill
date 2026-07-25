package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// flashText decodes the "<kind>:<urlescaped-msg>" krill_flash cookie value
// into its plain-text message, so a test can assert on the exact flash text
// (not just the ok:/err: prefix) when several error paths share a test.
func flashText(rec *httptest.ResponseRecorder) string {
	v := flashCookieValue(rec)
	_, raw, ok := strings.Cut(v, ":")
	if !ok {
		return ""
	}
	m, err := url.QueryUnescape(raw)
	if err != nil {
		return ""
	}
	return m
}

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

func TestSavePlacementBlockedWithOwnedVolume(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID, _, _ := rpFixture(t, h, q, orgSvc, "pl-owned@k.local")

	owner := "1000:0"
	if _, err := q.CreateVolume(ctx, db.CreateVolumeParams{
		ApplicationID: appID, Name: "data", MountPath: "/data", Owner: &owner,
	}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if _, err := q.CreateClusterNode(ctx, db.CreateClusterNodeParams{
		Name: "worker-1", SshHost: "10.0.0.2", SshPort: 22, SshUser: "root",
		SshKey: "k", HostKey: "", SwarmNodeID: "swarm-1",
	}); err != nil {
		t.Fatalf("create cluster node: %v", err)
	}

	// Switching to "any" while an owned volume exists on a multi-node cluster → err.
	if rec := postForm(t, h, base+"/placement", cookie, url.Values{"placement_mode": {"any"}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("any with owned volume + worker want 303+err, got %d", rec.Code)
	}
}

// TestSetDBInstanceNode exercises migrateDBInstanceNode's nil-engine metadata
// path — node pinning for a DB instance now lives on the org-level
// /db-servers page (moved off the env-level DB detail page in the
// logical-databases rework).
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

// TestMigrateDBInstanceNodeSameNode exercises migrateDBInstanceNode's
// same-node refusal under a live (non-nil) engine: noopEngine.Nodes returns
// no live nodes, so the only target that passes node validation is "" (the
// control-plane) — and the fixture instance also defaults to NodeHostname=""
// via CreateDBInstance, so posting node_hostname="" hits the same-node path
// (not invalid_node, and not a dispatched migration).
func TestMigrateDBInstanceNodeSameNode(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "dbmig-same@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "OrgDBMigSame")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-dbmigsame",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	if inst.NodeHostname != "" {
		t.Fatalf("fixture assumption broken: NodeHostname = %q, want empty", inst.NodeHostname)
	}
	cookie := loginAs(t, q, "dbmig-same@k.local")
	base := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(inst.ID)

	rec := postForm(t, h, base+"/node", cookie, url.Values{"node_hostname": {""}})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("same node want 303+err, got %d body %s", rec.Code, rec.Body.String())
	}
	if want := "The instance is already on that node."; flashText(rec) != want {
		t.Errorf("flash text = %q, want %q", flashText(rec), want)
	}
	// No migration was dispatched: node_hostname and status must be untouched.
	got, err := q.GetDBInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("get db instance: %v", err)
	}
	if got.NodeHostname != "" || got.Status != "idle" {
		t.Errorf("instance mutated by a rejected same-node request: node=%q status=%q", got.NodeHostname, got.Status)
	}
}

// TestMigrateDBInstanceNodeBusy exercises migrateDBInstanceNode's
// migrate-in-progress refusal: an instance whose status is already
// 'migrating' must reject any new node-change request outright — the busy
// check runs before the same-node check, so even a same-node target is
// refused with flash.err.migrate_in_progress rather than flash.err.same_node.
func TestMigrateDBInstanceNodeBusy(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "dbmig-busy@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "OrgDBMigBusy")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-dbmigbusy",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	if err := q.UpdateDBInstanceStatus(ctx, db.UpdateDBInstanceStatusParams{ID: inst.ID, Status: "migrating"}); err != nil {
		t.Fatalf("set status migrating: %v", err)
	}
	cookie := loginAs(t, q, "dbmig-busy@k.local")
	base := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(inst.ID)

	rec := postForm(t, h, base+"/node", cookie, url.Values{"node_hostname": {""}})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("busy instance want 303+err, got %d body %s", rec.Code, rec.Body.String())
	}
	if want := "A migration is already in progress for this instance."; flashText(rec) != want {
		t.Errorf("flash text = %q, want %q", flashText(rec), want)
	}
	got, err := q.GetDBInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("get db instance: %v", err)
	}
	if got.Status != "migrating" || got.NodeHostname != "" {
		t.Errorf("busy instance mutated by a rejected request: status=%q node=%q", got.Status, got.NodeHostname)
	}
}

// TestMigrateDBInstanceNodeSameTargetButDrifted covers the drift-healing case
// migrateDBInstanceNode's same-node check must let through: node_hostname
// metadata says "worker-1", but a running task is actually on a different
// node ("worker-2") — the metadata is stale (the legacy metadata-only node
// change), so re-selecting "worker-1" is the HEALING migration, not a no-op,
// and must be dispatched rather than refused as same_node.
func TestMigrateDBInstanceNodeSameTargetButDrifted(t *testing.T) {
	eng := nodesEngine{
		live:  []docker.SwarmNode{{ID: "n1", Hostname: "worker-1"}, {ID: "n2", Hostname: "worker-2"}},
		tasks: []docker.TaskPlacement{{NodeID: "n2", NodeName: "worker-2", State: "running"}},
	}
	h, q, orgSvc := newServerWithNodesEngine(t, eng)
	ctx := context.Background()
	ownerID := mkUser(t, q, "dbmig-drift@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "OrgDBMigDrift")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-dbmigdrift",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw", NodeHostname: "worker-1",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	cookie := loginAs(t, q, "dbmig-drift@k.local")
	base := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(inst.ID)

	// Target == metadata's node_hostname ("worker-1"), but the live task
	// contradicts it (running on "worker-2") — the handler must dispatch the
	// healing migration rather than refuse same_node.
	rec := postForm(t, h, base+"/node", cookie, url.Values{"node_hostname": {"worker-1"}})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("drifted same-node target want 303+ok (dispatch), got %d body %s", rec.Code, rec.Body.String())
	}
	if want := "Migration started — follow the progress in the instance log."; flashText(rec) != want {
		t.Errorf("flash text = %q, want %q", flashText(rec), want)
	}
}
