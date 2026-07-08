package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestMemberCannotCreateDBInstance(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memID := mkUser(t, q, "m@k.local")
	_, _ = q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memID, Role: "member"})
	cookie := loginAs(t, q, "m@k.local")

	form := url.Values{"engine": {"postgres"}, "name": {"pg1"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+i64(o.ID)+"/db-servers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member create instance want 403, got %d", rec.Code)
	}
}

func TestAdminCreatesDBInstance(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "o@k.local")

	form := url.Values{"engine": {"postgres"}, "name": {"shared-pg"}, "version": {"postgres:17"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+i64(o.ID)+"/db-servers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin create instance want 303, got %d", rec.Code)
	}
	insts, _ := q.ListDBInstancesByOrg(ctx, o.ID)
	if len(insts) != 1 || insts[0].Engine != "postgres" || insts[0].Superuser != "postgres" {
		t.Fatalf("expected 1 postgres instance, got %+v", insts)
	}
	if insts[0].SuperuserPassword == "" {
		t.Fatal("instance must get a generated password")
	}
}

func TestAdminCreatesDragonflyDBInstance(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "o@k.local")

	form := url.Values{"engine": {"dragonfly"}, "name": {"cache1"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+i64(o.ID)+"/db-servers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin create dragonfly instance want 303, got %d", rec.Code)
	}
	insts, _ := q.ListDBInstancesByOrg(ctx, o.ID)
	if len(insts) != 1 || insts[0].Engine != "dragonfly" {
		t.Fatalf("expected 1 dragonfly instance, got %+v", insts)
	}
	if !strings.Contains(insts[0].Image, "dragonfly") {
		t.Fatalf("expected default dragonfly image, got %q", insts[0].Image)
	}
	if insts[0].Superuser != "default" {
		t.Fatalf("expected superuser %q, got %q", "default", insts[0].Superuser)
	}
	if insts[0].SuperuserPassword == "" {
		t.Fatal("instance must get a generated password")
	}
}

func TestCreateDBInstanceRejectsUnknownEngine(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "o@k.local")

	form := url.Values{"engine": {"minio"}, "name": {"bucket1"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+i64(o.ID)+"/db-servers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want redirect back with a flash error, got %d", rec.Code)
	}
	insts, _ := q.ListDBInstancesByOrg(ctx, o.ID)
	if len(insts) != 0 {
		t.Fatalf("engine not yet registered must not create an instance, got %+v", insts)
	}
}

func TestDBInstanceCrossOrg404(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	aID := mkUser(t, q, "a@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, aID, "A")
	inst, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: orgA.ID, Engine: "postgres", Name: "pg", AppName: "krill-postgres-pg-zz",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	bID := mkUser(t, q, "b@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, bID, "B")
	cookieB := loginAs(t, q, "b@k.local")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/orgs/" + i64(orgB.ID) + "/db-servers/" + i64(inst.ID)},
		{http.MethodPost, "/orgs/" + i64(orgB.ID) + "/db-servers/" + i64(inst.ID) + "/stop"},
		{http.MethodPost, "/orgs/" + i64(orgB.ID) + "/db-servers/" + i64(inst.ID) + "/delete"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookieB)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("SECURITY FINDING: %s %s want 404, got %d", tc.method, tc.path, rec.Code)
		}
	}
}
