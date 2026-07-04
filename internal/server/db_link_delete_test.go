package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// TestDeleteLogicalDatabaseCascadesDBLinks verifies that dropping a logical
// database removes app_db_links pointing at it via the real FK
// (logical_database_id ... ON DELETE CASCADE) — deleteLogicalDatabase itself
// no longer calls any explicit link-cleanup query.
func TestDeleteLogicalDatabaseCascadesDBLinks(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "dbl-del@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "web.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "maindb-inst", AppName: "krill-postgres-maindb-inst",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "maindb", DbName: "app", Username: "postgres", Password: "pw",
	})
	if err != nil {
		t.Fatalf("create logical database: %v", err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{
		ApplicationID: app.ID, LogicalDatabaseID: &ldb.ID, VarName: "DATABASE_URL", Scheme: "postgres", Field: "url",
	}); err != nil {
		t.Fatalf("create db link: %v", err)
	}
	cookie := loginAs(t, q, "dbl-del@k.local")
	delURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) +
		"/databases/" + i64(ldb.ID) + "/delete"
	if rec := postForm(t, h, delURL, cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deleteLogicalDatabase: got %d, want 303", rec.Code)
	}
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 0 {
		t.Fatalf("db link should cascade-delete with its logical database, have %d", len(links))
	}
}
