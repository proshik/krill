package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// reloadCounts records how often each scheduler-reload hook fired.
type reloadCounts struct{ db, vol int }

// newServerCountingReloads mirrors newDeployServer but observes the two
// scheduler-reload hooks instead of discarding them.
func newServerCountingReloads(t *testing.T) (http.Handler, *db.Queries, *org.Service, *reloadCounts) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", AllowPrivateEgress: true}
	hub := deploy.NewLogHub()
	eng := noopEngine{}
	dep := deploy.New(eng, noopBuilder{}, deploy.NewDBStore(q), hub, "krill-net")
	dep.Start(context.Background())
	t.Cleanup(dep.Stop)
	dbSvc := dbservice.New(eng, dbservice.NewDBStore(q), hub, "krill-net")
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, dep, eng, hub, dbSvc)

	var counts reloadCounts
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q), true), func() { counts.db++ })
	return srv.Router(), q, orgSvc, &counts
}

// Deleting a logical database cascade-deletes its backup rows (FK ON DELETE
// CASCADE). Without a scheduler reload the cron entries for those rows keep
// firing against IDs that no longer exist, until some unrelated backup mutation
// happens to reload the schedule.
func TestDeleteLogicalDatabaseReloadsBackupScheduler(t *testing.T) {
	h, q, orgSvc, counts := newServerCountingReloads(t)
	ctx := context.Background()

	uid := mkUser(t, q, "reload-ldb@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	cookie := loginAs(t, q, "reload-ldb@k.local")

	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-rl1",
		Image: "postgres:17-alpine", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "app", DbName: "app", Username: "app", Password: "pw",
	})
	if err != nil {
		t.Fatalf("create logical db: %v", err)
	}
	dest, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "s3", Endpoint: "http://m:9000", Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if _, err := q.CreateBackup(ctx, db.CreateBackupParams{
		LogicalDatabaseID: ldb.ID, DestinationID: dest.ID, Schedule: "0 3 * * *", Retention: 7, Enabled: true,
	}); err != nil {
		t.Fatalf("create backup: %v", err)
	}

	before := counts.db
	delURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases/" + i64(ldb.ID) + "/delete"
	if rec := postForm(t, h, delURL, cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete want 303, got %d body %s", rec.Code, rec.Body.String())
	}
	if bs, _ := q.ListBackupsByLogicalDB(ctx, ldb.ID); len(bs) != 0 {
		t.Fatalf("backup rows not cascade-deleted: %d", len(bs))
	}
	if counts.db == before {
		t.Errorf("backup scheduler not reloaded after cascade delete (still %d): stale cron entries keep firing", counts.db)
	}
}

// Deleting an environment cascades to applications (→ app_volumes →
// volume_backups) and to logical databases (→ backups): both schedules go
// stale, so both hooks must fire.
func TestDeleteEnvironmentReloadsBackupScheduler(t *testing.T) {
	h, q, orgSvc, counts := newServerCountingReloads(t)
	ctx := context.Background()

	uid := mkUser(t, q, "reload-env@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	cookie := loginAs(t, q, "reload-env@k.local")

	inst, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-rl2",
		Image: "postgres:17-alpine", Superuser: "postgres", SuperuserPassword: "pw",
	})
	ldb, _ := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "app", DbName: "app", Username: "app", Password: "pw",
	})
	dest, _ := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "s3", Endpoint: "http://m:9000", Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})
	if _, err := q.CreateBackup(ctx, db.CreateBackupParams{
		LogicalDatabaseID: ldb.ID, DestinationID: dest.ID, Schedule: "0 3 * * *", Retention: 7, Enabled: true,
	}); err != nil {
		t.Fatalf("create backup: %v", err)
	}

	before := counts.db
	delURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/delete"
	if rec := postForm(t, h, delURL, cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete want 303, got %d body %s", rec.Code, rec.Body.String())
	}
	if counts.db == before {
		t.Errorf("backup scheduler not reloaded after environment delete (still %d)", counts.db)
	}
}
