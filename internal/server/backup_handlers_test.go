package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

// backupFixture creates an org with a postgres db_instance + logical database
// and an accessible MinIO destination, returning everything the backup
// handler tests need. Backups target a logical database
// (backups.logical_database_id FKs to logical_databases.id), and routing
// (loadBackupChain → loadLogicalDB) resolves {dbID} as the logical database's
// own id, so the returned id is ldb.ID.
func backupFixture(t *testing.T, q *db.Queries, orgSvc *org.Service) (org0 db.Organization, projID, envID, ldbID, destID int64, cookie *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")

	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "maindb-inst", AppName: "krill-postgres-maindb-inst",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "secret",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "maindb", DbName: "app", Username: "postgres", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create logical database: %v", err)
	}

	minio := testutil.NewMinio(t)
	dst := backup.Destination{Endpoint: minio.Endpoint, Bucket: "test", Region: minio.Region, AccessKey: minio.AccessKey, SecretKey: minio.SecretKey}
	if err := backup.CreateBucket(ctx, dst, true); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	d, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "primary", Endpoint: minio.Endpoint, Bucket: "test",
		Region: minio.Region, AccessKey: minio.AccessKey, SecretKey: minio.SecretKey,
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	return o, p.ID, e.ID, ldb.ID, d.ID, loginAs(t, q, "owner@k.local")
}

func backupsBase(orgID, projID, envID, ldbID int64) string {
	return "/orgs/" + i64(orgID) + "/projects/" + i64(projID) + "/environments/" + i64(envID) + "/databases/" + i64(ldbID) + "/backups"
}

func TestAddBackupSucceeds(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, ldbID, destID, cookie := backupFixture(t, q, orgSvc)

	base := backupsBase(o.ID, projID, envID, ldbID)
	form := url.Values{"destination_id": {i64(destID)}, "schedule_preset": {"custom"}, "schedule_custom": {"0 3 * * *"}, "retention": {"7"}, "prefix": {"daily"}}
	rec := postForm(t, h, base, cookie, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add backup want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	bks, err := q.ListBackupsByLogicalDB(ctx, ldbID)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(bks) != 1 || bks[0].DestinationID != destID || bks[0].Schedule != "0 3 * * *" || bks[0].Retention != 7 || !bks[0].Enabled {
		t.Fatalf("expected one enabled backup with the given config, got %+v", bks)
	}
}

func TestAddBackupCrossOrgDestinationFlash(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, ldbID, _, cookie := backupFixture(t, q, orgSvc)

	// A destination owned by a different org.
	otherOwner := mkUser(t, q, "other@k.local")
	otherOrg, _ := orgSvc.CreateOrg(ctx, otherOwner, "Other")
	otherDest, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: otherOrg.ID, Name: "x", Endpoint: "http://m:9000",
		Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})
	if err != nil {
		t.Fatalf("create other destination: %v", err)
	}

	base := backupsBase(o.ID, projID, envID, ldbID)
	form := url.Values{"destination_id": {i64(otherDest.ID)}, "schedule": {"0 3 * * *"}, "retention": {"7"}}
	rec := postForm(t, h, base, cookie, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cross-org destination want 303, got %d", rec.Code)
	}
	if !hasErrFlash(rec) {
		t.Fatalf("cross-org destination want err flash, got %q", flashCookieValue(rec))
	}
	if bks, _ := q.ListBackupsByLogicalDB(ctx, ldbID); len(bks) != 0 {
		t.Fatalf("expected no backup created, got %d", len(bks))
	}
}

func TestAddBackupZeroRetentionFlash(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, ldbID, destID, cookie := backupFixture(t, q, orgSvc)

	base := backupsBase(o.ID, projID, envID, ldbID)
	form := url.Values{"destination_id": {i64(destID)}, "schedule": {"0 3 * * *"}, "retention": {"0"}}
	rec := postForm(t, h, base, cookie, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("retention 0 want 303, got %d", rec.Code)
	}
	if !hasErrFlash(rec) {
		t.Fatalf("retention 0 want err flash, got %q", flashCookieValue(rec))
	}
	if bks, _ := q.ListBackupsByLogicalDB(ctx, ldbID); len(bks) != 0 {
		t.Fatalf("expected no backup created, got %d", len(bks))
	}
}

func TestToggleBackupFlipsEnabled(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, ldbID, destID, cookie := backupFixture(t, q, orgSvc)
	b, err := q.CreateBackup(ctx, db.CreateBackupParams{
		LogicalDatabaseID: ldbID, DestinationID: destID, Schedule: "0 3 * * *", Prefix: "", Retention: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}

	base := backupsBase(o.ID, projID, envID, ldbID)
	// A pause posted twice — a double click — stays paused: the form carries
	// the state it wants, not "flip".
	for i := 0; i < 2; i++ {
		rec := postForm(t, h, base+"/"+i64(b.ID)+"/toggle", cookie, url.Values{"enabled": {"0"}})
		if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
			t.Fatalf("toggle want 303, got %d %q", rec.Code, flashCookieValue(rec))
		}
		if got, _ := q.GetBackup(ctx, b.ID); got.Enabled {
			t.Fatalf("expected enabled=false after pause #%d, got true", i+1)
		}
	}
	// A form without the state (a page from before) changes nothing.
	if rec := postForm(t, h, base+"/"+i64(b.ID)+"/toggle", cookie, url.Values{}); !hasErrFlash(rec) {
		t.Fatal("a toggle without its desired state must be refused")
	}
	if got, _ := q.GetBackup(ctx, b.ID); got.Enabled {
		t.Fatal("a refused toggle must not flip the backup")
	}
}

func TestDeleteBackupRemovesIt(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, ldbID, destID, cookie := backupFixture(t, q, orgSvc)
	b, err := q.CreateBackup(ctx, db.CreateBackupParams{
		LogicalDatabaseID: ldbID, DestinationID: destID, Schedule: "0 3 * * *", Prefix: "", Retention: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}

	base := backupsBase(o.ID, projID, envID, ldbID)
	rec := postForm(t, h, base+"/"+i64(b.ID)+"/delete", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := q.GetBackup(ctx, b.ID); err == nil {
		t.Fatalf("backup should be deleted")
	}
}

// TestBackupCrossTenantIsolation verifies that loadBackupChain rejects an
// attempt by org-B to act on org-A's backup via org-B's DB chain.
// Each mutation route must return 404 and org-A's backup row must survive.
func TestBackupCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	// --- Org-A: owner userA, project, environment, instance, logical DB, destination, backup ---
	orgA, projA, envA, ldbA, destA, _ := backupFixture(t, q, orgSvc)
	aBackup, err := q.CreateBackup(ctx, db.CreateBackupParams{
		LogicalDatabaseID: ldbA,
		DestinationID:     destA,
		Schedule:          "0 2 * * *",
		Prefix:            "daily",
		Retention:         7,
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create org-A backup: %v", err)
	}
	aBackupID := aBackup.ID
	_ = orgA
	_ = projA
	_ = envA

	// --- Org-B: owner userB, project, environment, instance, logical DB ---
	userBID := mkUser(t, q, "userb-ct@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userBID, "OrgB-CT")
	projB, _ := orgSvc.CreateProject(ctx, orgB.ID, "ProjB", "")
	envB, _ := orgSvc.CreateEnvironment(ctx, projB.ID, "prod-b")
	instB, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: orgB.ID, Engine: "postgres", Name: "pg-b", AppName: "krill-postgres-pg-b",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "secret",
	})
	if err != nil {
		t.Fatalf("create org-B db instance: %v", err)
	}
	ldbB, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: instB.ID, EnvironmentID: envB.ID, Name: "db-b", DbName: "db_b", Username: "db_b", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create org-B logical database: %v", err)
	}
	cookieB := loginAs(t, q, "userb-ct@k.local")

	// Base path: org-B's full DB chain but with org-A's backup ID.
	base := backupsBase(orgB.ID, projB.ID, envB.ID, ldbB.ID)

	routes := []string{
		"/" + i64(aBackupID) + "/toggle",
		"/" + i64(aBackupID) + "/delete",
		"/" + i64(aBackupID) + "/run",
	}

	for _, suffix := range routes {
		target := base + suffix
		rec := postForm(t, h, target, cookieB, url.Values{})
		if rec.Code != http.StatusNotFound {
			t.Errorf("SECURITY FINDING: POST %s — expected 404 (cross-tenant isolation), got %d", target, rec.Code)
		}
	}

	// Org-A's backup row must still exist after all cross-tenant attempts.
	if _, err := q.GetBackup(ctx, aBackupID); err != nil {
		t.Fatalf("SECURITY FINDING: org-A backup row (id=%d) was mutated/deleted by cross-tenant request: %v", aBackupID, err)
	}
}

// Two configs on the same storage and prefix share one S3 directory, and each
// one's retention deletes the other's dumps; a double-submitted form is how
// the second usually appears.
func TestAddBackupRefusesSameStorageAndPrefix(t *testing.T) {
	h, q, orgSvc := newServer(t)
	o, projID, envID, ldbID, destID, cookie := backupFixture(t, q, orgSvc)
	base := backupsBase(o.ID, projID, envID, ldbID)
	form := url.Values{"destination_id": {i64(destID)}, "schedule_preset": {"daily"}, "retention": {"7"}, "prefix": {"daily"}}
	if rec := postForm(t, h, base, cookie, form); hasErrFlash(rec) {
		t.Fatalf("first add: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, h, base, cookie, form); !hasErrFlash(rec) {
		t.Fatal("a second config on the same storage and prefix must be refused")
	}
	form.Set("prefix", "weekly")
	if rec := postForm(t, h, base, cookie, form); hasErrFlash(rec) {
		t.Fatalf("another prefix is a separate directory and must be allowed: %q", flashCookieValue(rec))
	}
	if bks, _ := q.ListBackupsByLogicalDB(context.Background(), ldbID); len(bks) != 2 {
		t.Fatalf("want 2 backup configs, got %d", len(bks))
	}
}
