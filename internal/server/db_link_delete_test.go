package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// TestDeleteLogicalDatabaseLinks: the database page refuses to delete a
// database an app is linked to (as buckets are refused) and keeps the link;
// a database row removed another way (an environment delete) still takes its
// app_db_links with it through the real FK (logical_database_id ... ON
// DELETE CASCADE).
func TestDeleteLogicalDatabaseLinks(t *testing.T) {
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
	if rec := postForm(t, h, delURL, cookie, url.Values{}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("deleteLogicalDatabase of a linked database: got %d (error flash %v), want a refusal", rec.Code, hasErrFlash(rec))
	}
	if _, err := q.GetLogicalDatabase(ctx, ldb.ID); err != nil {
		t.Fatalf("the linked database must stay: %v", err)
	}
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 1 {
		t.Fatalf("the link must stay while the delete is refused, have %d", len(links))
	}
	if err := q.DeleteLogicalDatabase(ctx, ldb.ID); err != nil {
		t.Fatal(err)
	}
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 0 {
		t.Fatalf("db link should cascade-delete with its logical database, have %d", len(links))
	}
}
