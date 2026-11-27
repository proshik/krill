package server_test

import (
	"context"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// The Nodes page finds a worker's metrics by its Swarm node ID, through the
// name Krill registered it under (what the sampler files samples by), not by
// its display label: worker n2 is labelled "beta" but sampled as "alpha",
// and worker n3's label is "alpha", another worker's registered name.
func TestNodeLanesFindMetricsByNodeID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	orgSvc := org.NewService(q)
	eng := nodesEngine{live: []docker.SwarmNode{
		{ID: "n1", Hostname: "cp", Role: "manager", State: "ready", Leader: true},
		{ID: "n2", Hostname: "host-2", Role: "worker", State: "ready"},
		{ID: "n3", Hostname: "host-3", Role: "worker", State: "ready"},
	}}
	hub := deploy.NewLogHub()
	srv := server.New(config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}, auth.NewService(q), orgSvc, q, nil, eng, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q), true), func() {})
	srv.SetMetrics(metrics.NewDBStore(q))
	h := srv.Router()

	for _, w := range []struct{ name, swarm string }{{"alpha", "n2"}, {"gamma", "n3"}} {
		if _, err := q.CreateClusterNode(ctx, db.CreateClusterNodeParams{Name: w.name, SshHost: "192.0.2.1", SshPort: 22, SshUser: "root", SshKey: "x", HostKey: "x", SwarmNodeID: w.swarm}); err != nil {
			t.Fatal(err)
		}
	}
	for id, label := range map[string]string{"n2": "beta", "n3": "alpha"} {
		if err := q.UpsertNodeLabel(ctx, db.UpsertNodeLabelParams{SwarmNodeID: id, Label: label}); err != nil {
			t.Fatal(err)
		}
	}
	seedCapacity(t, q, "alpha", 4, 8<<30)
	seedMetric(t, pool, "alpha", "krill-1", 10, 6<<30)

	u := mkUser(t, q, "nm-op@k.local")
	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: u, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	o, _ := orgSvc.CreateOrg(ctx, u, "NodeMetrics")
	body := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/nodes", loginAs(t, q, "nm-op@k.local")).Body.String()
	lane := func(name string) string {
		i := strings.Index(body, `id="node-`+name+`"`)
		if i < 0 {
			t.Fatalf("no lane %s", name)
		}
		end := strings.Index(body[i+1:], `<section class="lane`)
		if end < 0 {
			return body[i:]
		}
		return body[i : i+1+end]
	}
	if !strings.Contains(lane("beta"), "6.0 GiB") {
		t.Error("worker n2 (label beta, sampled as alpha) lost its metrics")
	}
	if strings.Contains(lane("alpha"), "6.0 GiB") {
		t.Error("worker n3, labelled alpha, shows another worker's metrics")
	}
}
