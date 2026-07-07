package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFirewallPageRequiresInstanceAdmin verifies an ordinary org owner (not an
// instance operator) cannot reach the global worker-firewall routes: they 404,
// same as /nodes and /monitoring (the self-created-org privilege-escalation
// guard applies here too).
func TestFirewallPageRequiresInstanceAdmin(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ownerID := mkUser(t, q, "fw-nonadmin@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), ownerID, "OrgFWNIA")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	cookie := loginAs(t, q, "fw-nonadmin@k.local")
	base := "/orgs/" + i64(o.ID)

	req := httptest.NewRequest(http.MethodGet, base+"/firewall", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /firewall as non-instance-admin: want 404, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, base+"/firewall/lockdown", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /firewall/lockdown as non-instance-admin: want 404, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, base+"/firewall/open", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /firewall/open as non-instance-admin: want 404, got %d", rec.Code)
	}
}
