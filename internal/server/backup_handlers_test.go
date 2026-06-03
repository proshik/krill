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

// backupFixture creates an org with a postgres DB and an accessible MinIO
// destination, returning everything the backup handler tests need.
func backupFixture(t *testing.T, q *db.Queries, orgSvc *org.Service) (org0 db.Organization, projID, envID, pgID, destID int64, cookie *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	pg, err := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID: e.ID, Name: "maindb", AppName: "krill-postgres-maindb",
		DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: "secret", Image: "postgres:17",
	})
	if err != nil {
		t.Fatalf("create postgres: %v", err)
	}

	minio := testutil.NewMinio(t)
	dst := backup.Destination{Endpoint: minio.Endpoint, Bucket: "test", Region: minio.Region, AccessKey: minio.AccessKey, SecretKey: minio.SecretKey}
	if err := backup.CreateBucket(ctx, dst); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	d, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "primary", Endpoint: minio.Endpoint, Bucket: "test",
		Region: minio.Region, AccessKey: minio.AccessKey, SecretKey: minio.SecretKey,
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	return o, p.ID, e.ID, pg.ID, d.ID, loginAs(t, q, "owner@k.local")
}

func backupsBase(orgID, projID, envID, pgID int64) string {
	return "/orgs/" + i64(orgID) + "/projects/" + i64(projID) + "/environments/" + i64(envID) + "/databases/postgres/" + i64(pgID) + "/backups"
}

func TestAddBackupSucceeds(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, pgID, destID, cookie := backupFixture(t, q, orgSvc)

	base := backupsBase(o.ID, projID, envID, pgID)
	form := url.Values{"destination_id": {i64(destID)}, "schedule": {"0 3 * * *"}, "retention": {"7"}, "prefix": {"daily"}}
	rec := postForm(t, h, base, cookie, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add backup want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	bks, err := q.ListBackupsByDB(ctx, pgID)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(bks) != 1 || bks[0].DestinationID != destID || bks[0].Schedule != "0 3 * * *" || bks[0].Retention != 7 || !bks[0].Enabled {
		t.Fatalf("expected one enabled backup with the given config, got %+v", bks)
	}
}

func TestAddBackupCrossOrgDestination400(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, pgID, _, cookie := backupFixture(t, q, orgSvc)

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

	base := backupsBase(o.ID, projID, envID, pgID)
	form := url.Values{"destination_id": {i64(otherDest.ID)}, "schedule": {"0 3 * * *"}, "retention": {"7"}}
	rec := postForm(t, h, base, cookie, form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-org destination want 400, got %d", rec.Code)
	}
	if bks, _ := q.ListBackupsByDB(ctx, pgID); len(bks) != 0 {
		t.Fatalf("expected no backup created, got %d", len(bks))
	}
}

func TestAddBackupZeroRetention400(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, pgID, destID, cookie := backupFixture(t, q, orgSvc)

	base := backupsBase(o.ID, projID, envID, pgID)
	form := url.Values{"destination_id": {i64(destID)}, "schedule": {"0 3 * * *"}, "retention": {"0"}}
	rec := postForm(t, h, base, cookie, form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("retention 0 want 400, got %d", rec.Code)
	}
	if bks, _ := q.ListBackupsByDB(ctx, pgID); len(bks) != 0 {
		t.Fatalf("expected no backup created, got %d", len(bks))
	}
}

func TestToggleBackupFlipsEnabled(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, pgID, destID, cookie := backupFixture(t, q, orgSvc)
	b, err := q.CreateBackup(ctx, db.CreateBackupParams{
		PostgresDbID: pgID, DestinationID: destID, Schedule: "0 3 * * *", Prefix: "", Retention: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}

	base := backupsBase(o.ID, projID, envID, pgID)
	rec := postForm(t, h, base+"/"+i64(b.ID)+"/toggle", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("toggle want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	got, _ := q.GetBackup(ctx, b.ID)
	if got.Enabled {
		t.Fatalf("expected enabled=false after toggle, got true")
	}
}

func TestDeleteBackupRemovesIt(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	o, projID, envID, pgID, destID, cookie := backupFixture(t, q, orgSvc)
	b, err := q.CreateBackup(ctx, db.CreateBackupParams{
		PostgresDbID: pgID, DestinationID: destID, Schedule: "0 3 * * *", Prefix: "", Retention: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}

	base := backupsBase(o.ID, projID, envID, pgID)
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

	// --- Org-A: owner userA, project, environment, postgres DB, destination, backup ---
	orgA, projA, envA, pgA, destA, _ := backupFixture(t, q, orgSvc)
	aBackup, err := q.CreateBackup(ctx, db.CreateBackupParams{
		PostgresDbID:  pgA,
		DestinationID: destA,
		Schedule:      "0 2 * * *",
		Prefix:        "daily",
		Retention:     7,
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("create org-A backup: %v", err)
	}
	aBackupID := aBackup.ID
	_ = orgA
	_ = projA
	_ = envA

	// --- Org-B: owner userB, project, environment, postgres DB ---
	userBID := mkUser(t, q, "userb-ct@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userBID, "OrgB-CT")
	projB, _ := orgSvc.CreateProject(ctx, orgB.ID, "ProjB", "")
	envB, _ := orgSvc.CreateEnvironment(ctx, projB.ID, "prod-b")
	pgB, err := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID:    envB.ID,
		Name:             "db-b",
		AppName:          "krill-postgres-db-b",
		DatabaseName:     "app",
		DatabaseUser:     "postgres",
		DatabasePassword: "secret",
		Image:            "postgres:17",
	})
	if err != nil {
		t.Fatalf("create org-B postgres: %v", err)
	}
	cookieB := loginAs(t, q, "userb-ct@k.local")

	// Base path: org-B's full DB chain but with org-A's backup ID.
	base := backupsBase(orgB.ID, projB.ID, envB.ID, pgB.ID)

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
