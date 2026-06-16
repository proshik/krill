package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestDeleteDatabaseRemovesDBLinks(t *testing.T) {
	h, q, orgSvc := newDeployServer(t)
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
	_, _ = q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, Engine: "postgres", DbID: pg.ID, VarName: "DATABASE_URL", Scheme: "postgres"})
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
