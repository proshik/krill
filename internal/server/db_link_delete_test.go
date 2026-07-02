package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestDeleteDatabaseRemovesDBLinks(t *testing.T) {
	h, q, orgSvc, pool := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "dbl-del@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "web.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	pg, _ := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID: e.ID, Name: "maindb", AppName: "krill-postgres-maindb",
		DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: "pw", Image: "postgres:17",
	})
	// A legacy-style link (engine/db_id columns): CreateDBLink no longer writes
	// these (it only sets logical_database_id/instance_id), but the columns
	// still exist and deleteDatabase's cleanup still matches on them, so insert
	// one directly to exercise that legacy cleanup path.
	if _, err := pool.Exec(ctx, `INSERT INTO app_db_links (application_id, engine, db_id, var_name, scheme) VALUES ($1, $2, $3, $4, $5)`,
		app.ID, "postgres", pg.ID, "DATABASE_URL", "postgres"); err != nil {
		t.Fatalf("insert legacy db link: %v", err)
	}
	cookie := loginAs(t, q, "dbl-del@k.local")
	delURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) +
		"/databases/postgres/" + i64(pg.ID) + "/delete"
	if rec := postForm(t, h, delURL, cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deleteDatabase: got %d, want 303", rec.Code)
	}
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 0 {
		t.Fatalf("db link should be removed when its DB is deleted, have %d", len(links))
	}
}
