package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

// nodesOrg makes an org owned by email and returns its base URL + owner cookie + id.
// The owner is promoted to instance admin, since cluster-node management is now
// gated on that flag (not org-scoped RoleAdmin).
func nodesOrg(t *testing.T, q *db.Queries, orgSvc *org.Service, email, orgName string) (base string, cookie *http.Cookie, orgID int64) {
	t.Helper()
	ownerID := mkUser(t, q, email)
	o, err := orgSvc.CreateOrg(context.Background(), ownerID, orgName)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.SetUserAdmin(context.Background(), db.SetUserAdminParams{ID: ownerID, IsAdmin: true}); err != nil {
		t.Fatalf("set instance admin: %v", err)
	}
	return "/orgs/" + i64(o.ID), loginAs(t, q, email), o.ID
}

// TestNodesRequireInstanceAdmin verifies an ordinary org owner (not an instance
// operator) cannot reach the global cluster-node routes: they 404, closing the
// self-created-org privilege-escalation path.
func TestNodesRequireInstanceAdmin(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ownerID := mkUser(t, q, "node-nonadmin@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), ownerID, "OrgNIA")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	cookie := loginAs(t, q, "node-nonadmin@k.local")
	base := "/orgs/" + i64(o.ID)
	req, _ := http.NewRequest(http.MethodGet, base+"/nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nodes as non-instance-admin: want 404, got %d", rec.Code)
	}
	// A mutation is likewise blocked (and nothing is persisted).
	rec = postForm(t, h, base+"/nodes", cookie, url.Values{
		"name": {"w1"}, "ssh_host": {"10.0.0.2"}, "ssh_user": {"root"}, "ssh_key": {"KEY"},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /nodes as non-instance-admin: want 404, got %d", rec.Code)
	}
	if rows, _ := q.ListClusterNodes(context.Background()); len(rows) != 0 {
		t.Fatalf("expected no cluster_nodes rows, got %d", len(rows))
	}
}

func TestListNodesRenders(t *testing.T) {
	h, q, orgSvc := newServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-list@k.local", "OrgNL")
	req, _ := http.NewRequest(http.MethodGet, base+"/nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /nodes want 200, got %d", rec.Code)
	}
}

func TestAddNodeValidation(t *testing.T) {
	h, q, orgSvc := newServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-add@k.local", "OrgNA")

	// missing fields -> err flash
	if rec := postForm(t, h, base+"/nodes", cookie, url.Values{"name": {"w1"}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("missing fields want 303+err, got %d", rec.Code)
	}
	// all fields present but AdvertiseAddr unset in the test server -> err flash
	// (returns before any SSH attempt)
	full := url.Values{"name": {"w1"}, "ssh_host": {"10.0.0.2"}, "ssh_user": {"root"}, "ssh_key": {"KEY"}}
	if rec := postForm(t, h, base+"/nodes", cookie, full); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("advertise unset want 303+err, got %d", rec.Code)
	}
	// invalid ssh_port -> err flash
	bad := url.Values{"name": {"w1"}, "ssh_host": {"10.0.0.2"}, "ssh_user": {"root"}, "ssh_key": {"KEY"}, "ssh_port": {"abc"}}
	if rec := postForm(t, h, base+"/nodes", cookie, bad); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("bad port want 303+err, got %d", rec.Code)
	}
	// nothing persisted (all paths returned before create)
	if rows, _ := q.ListClusterNodes(context.Background()); len(rows) != 0 {
		t.Fatalf("expected no cluster_nodes rows, got %d", len(rows))
	}
}

func TestSetNodeAvailabilityInvalid(t *testing.T) {
	h, q, orgSvc := newServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-av@k.local", "OrgAV")
	rec := postForm(t, h, base+"/nodes/abc123/availability", cookie, url.Values{"availability": {"bogus"}})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("invalid availability want 303+err, got %d", rec.Code)
	}
}

// TestSetNodeLabel covers the display-label upsert/clear/validation. The test
// server wires a nil engine, so node-existence validation is skipped and the
// label is stored as given.
func TestSetNodeLabel(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-label@k.local", "OrgNLBL")
	const nid = "swarmnode-xyz"

	// set a label -> persisted
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {"worker-fra"}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("set label want 303 ok, got %d err=%v", rec.Code, hasErrFlash(rec))
	}
	labels, _ := q.ListNodeLabels(ctx)
	got := ""
	for _, l := range labels {
		if l.SwarmNodeID == nid {
			got = l.Label
		}
	}
	if got != "worker-fra" {
		t.Fatalf("label = %q, want worker-fra", got)
	}

	// too-long label -> err flash, value unchanged
	long := url.Values{"label": {strings.Repeat("x", 41)}}
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, long); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("too-long label want 303+err, got %d", rec.Code)
	}

	// empty label -> cleared (row deleted)
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {""}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("clear label want 303 ok, got %d", rec.Code)
	}
	labels, _ = q.ListNodeLabels(ctx)
	for _, l := range labels {
		if l.SwarmNodeID == nid {
			t.Fatalf("label not cleared: %q", l.Label)
		}
	}
}

func TestNodesAdminGate(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, _, orgID := nodesOrg(t, q, orgSvc, "nodes-gate-owner@k.local", "OrgNG")
	memberID := mkUser(t, q, "nodes-gate-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "nodes-gate-member@k.local")
	// Node routes are instance-operator only: a non-operator (here a plain org
	// member) gets 404 (not 403) so the capability is not even disclosed.
	for _, target := range []string{base + "/nodes", base + "/nodes/x/availability", base + "/nodes/x/remove", base + "/nodes/x/label"} {
		rec := postForm(t, h, target, mc, url.Values{"name": {"w"}, "ssh_host": {"h"}, "ssh_user": {"u"}, "ssh_key": {"k"}, "availability": {"drain"}})
		if rec.Code != http.StatusNotFound {
			t.Errorf("member POST %s want 404, got %d", target, rec.Code)
		}
	}
}
