package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
)

// seed a logical DB + a destination directly (no S3 check), return the URLs.
func seedBackupFixture(t *testing.T, q *db.Queries, orgSvc *org.Service, ownerEmail string) (orgID, projID, envID, dbID int64) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, ownerEmail)
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg1", AppName: "krill-postgres-pg1-bx",
		Image: "postgres:16", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	ld, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "appdb", DbName: "appdb", Username: "appdb", Password: "pw",
	})
	if err != nil {
		t.Fatalf("logical db: %v", err)
	}
	if _, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "s3", Endpoint: "http://x", Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	}); err != nil {
		t.Fatalf("destination: %v", err)
	}
	return o.ID, p.ID, e.ID, ld.ID
}

func destID(t *testing.T, q *db.Queries, orgID int64) int64 {
	t.Helper()
	ds, err := q.ListDestinationsByOrg(context.Background(), orgID)
	if err != nil || len(ds) == 0 {
		t.Fatalf("no destination: %v", err)
	}
	return ds[0].ID
}

func TestAddBackupPreset(t *testing.T) {
	h, q, orgSvc := newServer(t)
	orgID, projID, envID, dbID := seedBackupFixture(t, q, orgSvc, "bk-owner@k.local")
	cookie := loginAs(t, q, "bk-owner@k.local")
	dsID := destID(t, q, orgID)
	base := "/orgs/" + i64(orgID) + "/projects/" + i64(projID) + "/environments/" + i64(envID) + "/databases/" + i64(dbID) + "/backups"

	// daily -> "0 3 * * *"
	rec := postForm(t, h, base, cookie, url.Values{"destination_id": {i64(dsID)}, "schedule_preset": {"daily"}, "retention": {"7"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("daily: want 303, got %d: %s", rec.Code, rec.Body.String())
	}
	bks, _ := q.ListBackupsByLogicalDB(context.Background(), dbID)
	if len(bks) != 1 || bks[0].Schedule != "0 3 * * *" {
		t.Fatalf("daily backup schedule wrong: %+v", bks)
	}

	// off -> "" (on-demand), does not error
	rec = postForm(t, h, base, cookie, url.Values{"destination_id": {i64(dsID)}, "schedule_preset": {"off"}, "retention": {"7"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("off: want 303, got %d", rec.Code)
	}
	bks, _ = q.ListBackupsByLogicalDB(context.Background(), dbID)
	var hasEmpty bool
	for _, b := range bks {
		if b.Schedule == "" {
			hasEmpty = true
		}
	}
	if !hasEmpty {
		t.Errorf("off must create an on-demand backup with empty schedule, got %+v", bks)
	}

	// custom + bad cron -> no new backup (flash error)
	before := len(bks)
	postForm(t, h, base, cookie, url.Values{"destination_id": {i64(dsID)}, "schedule_preset": {"custom"}, "schedule_custom": {"not a cron"}, "retention": {"7"}})
	bks, _ = q.ListBackupsByLogicalDB(context.Background(), dbID)
	if len(bks) != before {
		t.Errorf("bad custom cron must not create a backup")
	}
}

func TestSafeReturnPath(t *testing.T) {
	r, _ := http.NewRequest("GET", "http://localhost/x", nil)
	cases := map[string]string{
		"/orgs/1/projects/2/databases/3": "/orgs/1/projects/2/databases/3",
		"/orgs/1?tab=volumes":            "/orgs/1?tab=volumes",
		"//evil.com/x":                   "",
		"https://evil.com":               "",
		"not-a-path":                     "",
		"":                               "",
	}
	for in, want := range cases {
		if got := server.SafeReturnPathForTest(r, in); got != want {
			t.Errorf("SafeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}
