package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
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
