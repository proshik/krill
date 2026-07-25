package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/oplock"
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

	form := url.Values{"engine": {"mysql"}, "name": {"bucket1"}}
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
		t.Fatalf("unregistered engine must not create an instance, got %+v", insts)
	}
}

func TestAdminCreatesMinioDBInstance(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "o@k.local")

	form := url.Values{"engine": {"minio"}, "name": {"bucket1"}, "root_user": {"minioadmin"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+i64(o.ID)+"/db-servers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin create minio instance want 303, got %d", rec.Code)
	}
	insts, _ := q.ListDBInstancesByOrg(ctx, o.ID)
	if len(insts) != 1 || insts[0].Engine != "minio" {
		t.Fatalf("expected 1 minio instance, got %+v", insts)
	}
	if insts[0].Superuser != "minioadmin" {
		t.Fatalf("expected superuser %q, got %q", "minioadmin", insts[0].Superuser)
	}
	if insts[0].SuperuserPassword == "" {
		t.Fatal("instance must get a generated password")
	}
	if !strings.Contains(insts[0].Image, "minio") {
		t.Fatalf("expected default minio image, got %q", insts[0].Image)
	}
}

func TestCreateMinioDBInstanceRejectsBadRootUser(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "o@k.local")

	for _, root := range []string{"", "ab", "has spaces", "has-dash", strings.Repeat("x", 64)} {
		form := url.Values{"engine": {"minio"}, "name": {"bucket1"}, "root_user": {root}}
		req := httptest.NewRequest(http.MethodPost, "/orgs/"+i64(o.ID)+"/db-servers", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("root_user %q: want redirect back with a flash error, got %d", root, rec.Code)
		}
	}
	insts, _ := q.ListDBInstancesByOrg(ctx, o.ID)
	if len(insts) != 0 {
		t.Fatalf("bad root_user must not create an instance, got %+v", insts)
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

// TestResetMigratingInstances covers the boot sweep (Fix B): a row stuck at
// status='migrating' (a control-plane restart killed the in-process job,
// whose oplock is in-memory and so is gone on the new process) is swept to
// 'error' rather than staying wedged forever.
func TestResetMigratingInstances(t *testing.T) {
	_, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "reset-mig@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "OrgResetMig")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg", AppName: "krill-postgres-resetmig",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	if err := q.UpdateDBInstanceStatus(ctx, db.UpdateDBInstanceStatusParams{ID: inst.ID, Status: "migrating"}); err != nil {
		t.Fatalf("set status migrating: %v", err)
	}
	if err := q.ResetMigratingInstances(ctx); err != nil {
		t.Fatalf("reset migrating instances: %v", err)
	}
	got, err := q.GetDBInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("get db instance: %v", err)
	}
	if got.Status != "error" {
		t.Errorf("status after boot sweep = %q, want %q", got.Status, "error")
	}
}

// TestDBInstanceLifecycleGuardedDuringMigration verifies every admin POST
// handler that mutates a DB instance's service/volume — deploy/start/stop/
// version/external-port/delete — refuses while a migration holds the
// instance's in-memory oplock (internal/oplock), regardless of what the DB
// row's status column says. A mutation mid-copy can restart the DB onto the
// source volume while tar reads it (torn pages in the target copy), or —
// with delete — destroy the still-intact source outright.
func TestDBInstanceLifecycleGuardedDuringMigration(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "dbmig-guard@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "OrgDBMigGuard")
	cookie := loginAs(t, q, "dbmig-guard@k.local")

	routes := []struct {
		name string
		path string
		form url.Values
	}{
		{"deploy", "/deploy", url.Values{}},
		{"start", "/start", url.Values{}},
		{"stop", "/stop", url.Values{}},
		{"version", "/version", url.Values{"image": {"postgres:18"}}},
		{"external-port", "/external-port", url.Values{"external_port": {"5433"}}},
		{"delete", "/delete", url.Values{}},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
				OrganizationID: o.ID, Engine: "postgres", Name: "pg-" + rt.name,
				AppName: "krill-postgres-guard-" + rt.name, Image: "postgres:17",
				Superuser: "postgres", SuperuserPassword: "pw",
			})
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}
			lock := oplock.DBInstance(inst.AppName)
			if !oplock.TryAcquire(lock) {
				t.Fatal("setup: acquire oplock")
			}
			defer oplock.Release(lock)

			base := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(inst.ID)
			rec := postForm(t, h, base+rt.path, cookie, rt.form)
			if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
				t.Fatalf("%s: want 303+err, got %d body %s", rt.name, rec.Code, rec.Body.String())
			}
			if want := "A migration is already in progress for this instance."; flashText(rec) != want {
				t.Errorf("%s: flash text = %q, want %q", rt.name, flashText(rec), want)
			}
			got, err := q.GetDBInstance(ctx, inst.ID)
			if err != nil {
				t.Fatalf("%s: instance row missing after a guarded request (should be untouched): %v", rt.name, err)
			}
			if got.Status != "idle" || got.Image != "postgres:17" || got.ExternalPort != nil {
				t.Errorf("%s: instance mutated by a guarded request: status=%q image=%q ext=%v", rt.name, got.Status, got.Image, got.ExternalPort)
			}
		})
	}
}
