package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

// nodesOrg makes an org owned by email and returns its base URL + owner cookie + id.
func nodesOrg(t *testing.T, q *db.Queries, orgSvc *org.Service, email, orgName string) (base string, cookie *http.Cookie, orgID int64) {
	t.Helper()
	ownerID := mkUser(t, q, email)
	o, err := orgSvc.CreateOrg(context.Background(), ownerID, orgName)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return "/orgs/" + i64(o.ID), loginAs(t, q, email), o.ID
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

func TestNodesAdminGate(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, _, orgID := nodesOrg(t, q, orgSvc, "nodes-gate-owner@k.local", "OrgNG")
	memberID := mkUser(t, q, "nodes-gate-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "nodes-gate-member@k.local")
	for _, target := range []string{base + "/nodes", base + "/nodes/x/availability", base + "/nodes/x/remove"} {
		rec := postForm(t, h, target, mc, url.Values{"name": {"w"}, "ssh_host": {"h"}, "ssh_user": {"u"}, "ssh_key": {"k"}, "availability": {"drain"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s want 403, got %d", target, rec.Code)
		}
	}
}
