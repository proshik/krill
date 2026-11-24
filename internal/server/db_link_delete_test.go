package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/oplock"
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

// A link created while the delete is waiting for the database's lock is seen
// by the delete's own check: it refuses instead of dropping the new link.
func TestDeleteLogicalDatabaseWaitsForLinkCreation(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "dbl-race@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "web-race.x", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "race-inst", AppName: "krill-postgres-race-inst",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "racedb", DbName: "racedb", Username: "racedb", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A link creation in progress holds the database's lock.
	key := oplock.LogicalDB(ldb.ID)
	if err := oplock.Acquire(ctx, key); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		done <- postForm(t, h, "/orgs/"+i64(o.ID)+"/projects/"+i64(p.ID)+"/environments/"+i64(e.ID)+"/databases/"+i64(ldb.ID)+"/delete", loginAs(t, q, "dbl-race@k.local"), url.Values{})
	}()
	select {
	case <-done:
		t.Fatal("the delete went ahead while a link creation held the database's lock")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, LogicalDatabaseID: &ldb.ID, VarName: "DATABASE_URL", Scheme: "postgres", Field: "url"}); err != nil {
		t.Fatal(err)
	}
	oplock.Release(key)
	if rec := <-done; !hasErrFlash(rec) {
		t.Fatalf("the delete must refuse the database linked meanwhile, got %d", rec.Code)
	}
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 1 {
		t.Fatalf("the new link must survive, have %d", len(links))
	}
}

// Deleting a Postgres server together with its databases bypassed the
// database's own link check: the server is refused while apps link to it.
func TestDeleteDBInstanceRefusedWhileLinked(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "dbi-linked@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "web-dbi.x", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "linked-inst", AppName: "krill-postgres-linked-inst",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "shop", DbName: "shop", Username: "shop", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, LogicalDatabaseID: &ldb.ID, VarName: "DATABASE_URL", Scheme: "postgres", Field: "url"}); err != nil {
		t.Fatal(err)
	}
	rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/db-servers/"+i64(inst.ID)+"/delete", loginAs(t, q, "dbi-linked@k.local"), url.Values{"delete_databases": {"on"}})
	if !hasErrFlash(rec) {
		t.Fatalf("deleting a server whose database an app links must be refused, got %d", rec.Code)
	}
	if _, err := q.GetDBInstance(ctx, inst.ID); err != nil {
		t.Fatalf("the server must stay: %v", err)
	}
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 1 {
		t.Fatalf("the link must stay, have %d", len(links))
	}
}
