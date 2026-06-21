package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

func TestMonitoringMemberForbidden(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	owner := mkUser(t, q, "mon-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "Org")
	mem := mkUser(t, q, "mon-mem@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: mem, Role: "member"}); err != nil {
		t.Fatalf("member: %v", err)
	}
	cookie := loginAs(t, q, "mon-mem@k.local")
	for _, p := range []string{"/monitoring", "/monitoring/data?range=24h"} {
		req := httptest.NewRequest(http.MethodGet, "/orgs/"+i64(o.ID)+p, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: member want 403, got %d", p, rec.Code)
		}
	}
}

// monitoringFixture creates an org with an admin owner, wires a real metrics
// store into a new server, and returns the org base path + the owner's cookie.
func monitoringFixture(t *testing.T, q *db.Queries, orgSvc *org.Service) (string, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, "mon-admin@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "MonOrg")
	return "/orgs/" + i64(o.ID), loginAs(t, q, "mon-admin@k.local")
}

// newMonServer returns an http.Handler backed by a real DB, with the metrics
// store wired in so /monitoring/data can query samples + capacity.
func newMonServer(t *testing.T) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	dbSvc := dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net")
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbSvc)
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q)), func() {})
	srv.SetMetrics(metrics.NewDBStore(q))
	return srv.Router(), q, orgSvc
}

// seedMetric inserts one metric sample for the given node and component.
func seedMetric(t *testing.T, q *db.Queries, node, component string, cpu float64, mem int64) {
	t.Helper()
	st := metrics.NewDBStore(q)
	if err := st.Insert(context.Background(), node, component, cpu, mem, 0); err != nil {
		t.Fatalf("seedMetric node=%s comp=%s: %v", node, component, err)
	}
}

// seedCapacity upserts node capacity for the given node.
func seedCapacity(t *testing.T, q *db.Queries, node string, ncpu int, memTotal int64) {
	t.Helper()
	st := metrics.NewDBStore(q)
	if err := st.UpsertCapacity(context.Background(), node, ncpu, memTotal); err != nil {
		t.Fatalf("seedCapacity node=%s: %v", node, err)
	}
}

// getWithCookie performs a GET request with the given cookie and returns the recorder.
func getWithCookie(t *testing.T, h http.Handler, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMonitoringDataPerNode(t *testing.T) {
	h, q, orgSvc := newMonServer(t)
	// minimal org + login (owner = admin role)
	base, cookie := monitoringFixture(t, q, orgSvc)

	// seed two nodes' latest samples + capacity via the metrics store
	seedMetric(t, q, "cp", "krill", 10, 100<<20)
	seedMetric(t, q, "w1", "app-svc", 30, 300<<20)
	seedCapacity(t, q, "cp", 2, 1<<30)
	seedCapacity(t, q, "w1", 4, 2<<30)

	rec := getWithCookie(t, h, base+"/monitoring/data?range=1h", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Nodes []struct {
			Node     string `json:"node"`
			NCPU     int    `json:"ncpu"`
			MemTotal int64  `json:"mem_total"`
		} `json:"nodes"`
		Rows []struct {
			Node string `json:"node"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]bool{}
	for _, n := range d.Nodes {
		nodes[n.Node] = true
	}
	if !nodes["cp"] || !nodes["w1"] {
		t.Fatalf("expected both nodes in summary, got %v", nodes)
	}
	rowNodes := map[string]bool{}
	for _, r := range d.Rows {
		rowNodes[r.Node] = true
	}
	if !rowNodes["cp"] || !rowNodes["w1"] {
		t.Fatalf("expected node-tagged rows, got %v", rowNodes)
	}
}
