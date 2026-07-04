package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// nodesEngine is a noopEngine that reports a fixed set of live Swarm nodes, so
// node-reconciliation handlers (savePlacement's live-node filter, removeNode's
// preflight/removal) have a real live-node set to check submitted/pinned node
// IDs against.
type nodesEngine struct {
	noopEngine
	live []docker.SwarmNode
}

func (e nodesEngine) Nodes(context.Context) ([]docker.SwarmNode, error) { return e.live, nil }

// newServerWithNodesEngine mirrors newDeployServer but injects a nodesEngine
// reporting a fixed live-node set instead of a plain noopEngine.
func newServerWithNodesEngine(t *testing.T, eng nodesEngine) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	dep := deploy.New(eng, noopBuilder{}, deploy.NewDBStore(q), hub, "krill-net")
	dep.Start(context.Background())
	t.Cleanup(dep.Stop)
	dbSvc := dbservice.New(eng, dbservice.NewDBStore(q), hub, "krill-net")
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, dep, eng, hub, dbSvc)
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q)), func() {})
	return srv.Router(), q, orgSvc
}

// TestSavePlacementFilterLiveNodes covers the savePlacement live-node filter
// with a real (fake) engine reporting a fixed live-node set: submitted node
// IDs not present in the live set are silently dropped, and a submission that
// contains ONLY removed IDs is rejected outright (leaving the prior value in
// place) rather than silently persisting an empty/dead placement.
func TestSavePlacementFilterLiveNodes(t *testing.T) {
	eng := nodesEngine{live: []docker.SwarmNode{{ID: "live-1"}, {ID: "live-2"}}}
	h, q, orgSvc := newServerWithNodesEngine(t, eng)
	ctx := context.Background()
	base, cookie, appID, _, _ := rpFixture(t, h, q, orgSvc, "pl-filter@k.local")

	// pin with one live + one removed node -> the removed one is dropped, only
	// the live one persists.
	rec := postForm(t, h, base+"/placement", cookie, url.Values{
		"placement_mode":  {"pin"},
		"placement_nodes": {"live-1", "removed-99"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("pin with one live + one removed node: want 303, got %d", rec.Code)
	}
	app, err := q.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if app.PlacementNodes != "live-1" {
		t.Fatalf("placement_nodes = %q, want live-1 only (removed dropped)", app.PlacementNodes)
	}

	// pin with ONLY a removed node -> rejected, placement stays unchanged.
	rec = postForm(t, h, base+"/placement", cookie, url.Values{
		"placement_mode":  {"pin"},
		"placement_nodes": {"removed-99"},
	})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("pin with only a removed node: want 303+err, got %d", rec.Code)
	}
	app, err = q.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	if app.PlacementNodes != "live-1" {
		t.Fatalf("placement_nodes changed after a rejected update: got %q, want unchanged live-1", app.PlacementNodes)
	}
}

// TestRemoveNodeConfirmThenForce covers the removeNode preflight: a DB
// instance still pinned to the node being removed makes the first (no-force)
// POST render the confirm page (200, instance name in the body) and leave the
// cluster_nodes row untouched; a second POST with force=1 actually removes
// the node and deletes the row.
func TestRemoveNodeConfirmThenForce(t *testing.T) {
	eng := nodesEngine{live: []docker.SwarmNode{{ID: "node-1", Hostname: "worker-1"}}}
	h, q, orgSvc := newServerWithNodesEngine(t, eng)
	ctx := context.Background()
	base, cookie, orgID := nodesOrg(t, q, orgSvc, "node-confirm@k.local", "OrgNC")

	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: orgID, Engine: "postgres", Name: "pinned-db", AppName: "krill-postgres-nc",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	if err := q.SetDBInstanceNode(ctx, db.SetDBInstanceNodeParams{ID: inst.ID, NodeHostname: "worker-1"}); err != nil {
		t.Fatalf("pin db instance: %v", err)
	}
	row, err := q.CreateClusterNode(ctx, db.CreateClusterNodeParams{
		Name: "worker-1", SshHost: "10.0.0.9", SshPort: 22, SshUser: "root",
		SshKey: "enc-key", HostKey: "hostkey", SwarmNodeID: "node-1",
	})
	if err != nil {
		t.Fatalf("create cluster node row: %v", err)
	}

	// Without force: the DB instance is still pinned here, so removeNode must
	// warn instead of silently orphaning it.
	rec := postForm(t, h, base+"/nodes/node-1/remove", cookie, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove without force: want 200 (confirm rendered), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pinned-db") {
		t.Fatalf("confirm body missing pinned instance name %q: %s", "pinned-db", rec.Body.String())
	}
	rows, err := q.ListClusterNodes(ctx)
	if err != nil {
		t.Fatalf("list cluster nodes: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == row.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("cluster_nodes row %d removed before an explicit force confirm", row.ID)
	}

	// With force=1: the node is actually removed and the row deleted.
	rec = postForm(t, h, base+"/nodes/node-1/remove", cookie, url.Values{"force": {"1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("remove with force=1: want 303, got %d", rec.Code)
	}
	rows, err = q.ListClusterNodes(ctx)
	if err != nil {
		t.Fatalf("list cluster nodes: %v", err)
	}
	for _, r := range rows {
		if r.ID == row.ID {
			t.Fatalf("cluster_nodes row %d still present after force remove", row.ID)
		}
	}
}
