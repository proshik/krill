package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestMemberCannotCreateDatabase(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg", AppName: "krill-postgres-pg-mem",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "secret",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	memberID := mkUser(t, q, "m@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	cookie := loginAs(t, q, "m@k.local")

	form := url.Values{"instance_id": {i64(inst.ID)}, "name": {"db"}}
	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"
	rec := postForm(t, h, target, cookie, form)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member create db want 403, got %d", rec.Code)
	}
}

// TestAdminCreatesLogicalDatabase exercises createLogicalDatabase end-to-end
// against a nil-engine server: dbservice.InstanceRunning returns true and
// ProvisionLogicalDB is a no-op with a nil engine (tests only), so the row is
// created without a live postgres container.
func TestAdminCreatesLogicalDatabase(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg", AppName: "krill-postgres-pg-admin",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "secret",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	if err := q.UpdateDBInstanceStatus(ctx, db.UpdateDBInstanceStatusParams{ID: inst.ID, Status: "running"}); err != nil {
		t.Fatalf("mark instance running: %v", err)
	}
	cookie := loginAs(t, q, "o@k.local")

	form := url.Values{"instance_id": {i64(inst.ID)}, "name": {"maindb"}}
	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"
	rec := postForm(t, h, target, cookie, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin create want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	rows, err := q.ListLogicalDatabasesByEnvironment(ctx, e.ID)
	if err != nil {
		t.Fatalf("list logical databases: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "maindb" || rows[0].DbName != "maindb" {
		t.Fatalf("expected 1 logical db named maindb, got %+v", rows)
	}
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }

func TestDatabaseCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	// --- Org-A: owner userA, project, environment, instance, logical DB ---
	userAID := mkUser(t, q, "usera@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userAID, "OrgA")
	projA, _ := orgSvc.CreateProject(ctx, orgA.ID, "ProjA", "")
	envA, _ := orgSvc.CreateEnvironment(ctx, projA.ID, "prod-a")

	instA, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: orgA.ID, Engine: "postgres", Name: "pg-a", AppName: "krill-postgres-pg-a",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "secret",
	})
	if err != nil {
		t.Fatalf("create org-A db instance: %v", err)
	}
	ldbA, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: instA.ID, EnvironmentID: envA.ID, Name: "db-a", DbName: "db_a", Username: "db_a", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create org-A logical database: %v", err)
	}
	aDBID := ldbA.ID

	// --- Org-B: owner userB, project, environment ---
	userBID := mkUser(t, q, "userb@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userBID, "OrgB")
	projB, _ := orgSvc.CreateProject(ctx, orgB.ID, "ProjB", "")
	envB, _ := orgSvc.CreateEnvironment(ctx, projB.ID, "prod-b")

	cookieB := loginAs(t, q, "userb@k.local")

	// Base path: org-B's chain but with org-A's dbID
	basePath := "/orgs/" + i64(orgB.ID) +
		"/projects/" + i64(projB.ID) +
		"/environments/" + i64(envB.ID) +
		"/databases/" + i64(aDBID)

	cases := []struct {
		method string
		suffix string
	}{
		{http.MethodGet, ""},
		{http.MethodPost, "/delete"},
	}

	for _, tc := range cases {
		target := basePath + tc.suffix
		req := httptest.NewRequest(tc.method, target, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookieB)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			// Any non-404 is a security finding — report loudly.
			t.Errorf("SECURITY FINDING: %s %s — expected 404 (cross-tenant isolation), got %d", tc.method, target, rec.Code)
		}
	}

	// After all attempts (including delete), the org-A logical DB row must still exist.
	if _, err := q.GetLogicalDatabase(ctx, aDBID); err != nil {
		t.Fatalf("SECURITY FINDING: org-A logical database row (id=%d) was deleted by cross-tenant request: %v", aDBID, err)
	}
}

// TestEnvDatabasesNewServerLink checks that an environment's databases tab
// offers admins a way to create an org-level server, and hides it from members.
func TestEnvDatabasesNewServerLink(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	memberID := mkUser(t, q, "m@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"
	link := `href="/orgs/` + i64(o.ID) + `/db-servers?new=server"`

	rec := getWithCookie(t, h, target, loginAs(t, q, "o@k.local"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), link) {
		t.Fatalf("admin page lacks the new-server link %s", link)
	}

	rec = getWithCookie(t, h, target, loginAs(t, q, "m@k.local"))
	if rec.Code != http.StatusOK {
		t.Fatalf("member GET want 200, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), link) {
		t.Fatalf("member page shows the new-server link %s", link)
	}
}

// TestEnvPageOrgTabsRow checks that inside an environment the organization's
// tabs get their own row, gated for the viewer as on the organization's pages,
// and that an organization's own page has no such row.
func TestEnvPageOrgTabsRow(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "o@k.local")
	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: ownerID, IsAdmin: true}); err != nil {
		t.Fatalf("set instance admin: %v", err)
	}
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	memberID := mkUser(t, q, "m@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	base := "/orgs/" + i64(o.ID)
	envPage := base + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"

	row := func(body string) string {
		start := strings.Index(body, `<nav class="orgrow"`)
		if start < 0 {
			return ""
		}
		end := strings.Index(body[start:], "</nav>")
		return body[start : start+end]
	}
	cases := []struct {
		email     string
		has, lack []string
	}{
		{"o@k.local", []string{"", "/db-servers", "/nodes", "/topology", "/monitoring", "/settings"}, nil},
		{"m@k.local", []string{"", "/db-servers", "/settings"}, []string{"/nodes", "/topology", "/monitoring"}},
	}
	for _, c := range cases {
		cookie := loginAs(t, q, c.email)
		rec := getWithCookie(t, h, envPage, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: env page want 200, got %d", c.email, rec.Code)
		}
		r := row(rec.Body.String())
		if r == "" {
			t.Fatalf("%s: env page has no organization tabs row", c.email)
		}
		for _, path := range c.has {
			if !strings.Contains(r, `href="`+base+path+`"`) {
				t.Errorf("%s: row lacks %q", c.email, base+path)
			}
		}
		for _, path := range c.lack {
			if strings.Contains(r, `href="`+base+path+`"`) {
				t.Errorf("%s: row shows %q", c.email, base+path)
			}
		}
		if rec := getWithCookie(t, h, base+"/db-servers", cookie); row(rec.Body.String()) != "" {
			t.Errorf("%s: an organization page must not have the row", c.email)
		}
	}
}
