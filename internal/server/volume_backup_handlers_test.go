package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

// volumeBackupFixture builds an org→proj→env→app→app_volume plus an accessible
// MinIO destination in the SAME org, returning everything the volume-backup
// handler tests need.
func volumeBackupFixture(t *testing.T, q *db.Queries, orgSvc *org.Service) (org0 db.Organization, appID, volID, destID int64, cookie *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, "vbk-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	v, err := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"})
	if err != nil {
		t.Fatalf("create volume: %v", err)
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
	return o, app.ID, v.ID, d.ID, loginAs(t, q, "vbk-owner@k.local")
}

func appBase(orgID, appID int64, q *db.Queries) string {
	// Resolve the proj/env from the app to build the full chain URL.
	app, _ := q.GetApplication(context.Background(), appID)
	env, _ := q.GetEnvironment(context.Background(), app.EnvironmentID)
	return "/orgs/" + i64(orgID) + "/projects/" + i64(env.ProjectID) +
		"/environments/" + i64(env.ID) + "/apps/" + i64(appID)
}

func TestAddVolumeBackupHappyPath(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	o, appID, volID, destID, cookie := volumeBackupFixture(t, q, orgSvc)

	base := appBase(o.ID, appID, q)
	form := url.Values{"destination_id": {i64(destID)}, "schedule_preset": {"custom"}, "schedule_custom": {"0 3 * * *"}, "retention": {"7"}, "prefix": {"daily"}}
	rec := postForm(t, h, base+"/volumes/"+i64(volID)+"/backups", cookie, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add volume backup want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	bks, err := q.ListVolumeBackupsByVolume(ctx, volID)
	if err != nil {
		t.Fatalf("list volume backups: %v", err)
	}
	if len(bks) != 1 || bks[0].DestinationID != destID || bks[0].Schedule != "0 3 * * *" || bks[0].Retention != 7 || !bks[0].Enabled {
		t.Fatalf("expected one enabled volume backup with the given config, got %+v", bks)
	}

	// A destination owned by a different org must be rejected with a flash and no row.
	otherOwner := mkUser(t, q, "vbk-other@k.local")
	otherOrg, _ := orgSvc.CreateOrg(ctx, otherOwner, "Other")
	otherDest, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: otherOrg.ID, Name: "x", Endpoint: "http://m:9000",
		Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})
	if err != nil {
		t.Fatalf("create other destination: %v", err)
	}
	form2 := url.Values{"destination_id": {i64(otherDest.ID)}, "schedule": {"0 3 * * *"}, "retention": {"7"}}
	rec2 := postForm(t, h, base+"/volumes/"+i64(volID)+"/backups", cookie, form2)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("cross-org destination want 303, got %d", rec2.Code)
	}
	if !hasErrFlash(rec2) {
		t.Fatalf("cross-org destination want err flash, got %q", flashCookieValue(rec2))
	}
	if bks, _ := q.ListVolumeBackupsByVolume(ctx, volID); len(bks) != 1 {
		t.Fatalf("expected still one volume backup after cross-org attempt, got %d", len(bks))
	}

	// Regression guard: a flat management sub-route must work for the legit
	// owner (the dead-route bug made every such route 404 unconditionally).
	vb := bks[0]
	want := "1"
	if vb.Enabled {
		want = "0"
	}
	toggleRec := postForm(t, h, base+"/volumes/backups/"+i64(vb.ID)+"/toggle", cookie, url.Values{"enabled": {want}})
	if toggleRec.Code != http.StatusSeeOther {
		t.Fatalf("toggle volume backup want 303, got %d (%s)", toggleRec.Code, toggleRec.Body.String())
	}
	after, err := q.GetVolumeBackup(ctx, vb.ID)
	if err != nil {
		t.Fatalf("get volume backup after toggle: %v", err)
	}
	if after.Enabled == vb.Enabled {
		t.Fatalf("expected enabled to flip from %v, got %v", vb.Enabled, after.Enabled)
	}
}

// TestVolumeBackupCrossTenantIsolation verifies that loadVolumeBackupChain
// rejects an attempt by org-B to act on org-A's volume backup via org-B's app
// chain: the mutation must 404 and org-A's row must survive.
func TestVolumeBackupCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()

	// --- Org-A: app + volume + volume_backup ---
	oA, appA, volA, destA, _ := volumeBackupFixture(t, q, orgSvc)
	aBackup, err := q.CreateVolumeBackup(ctx, db.CreateVolumeBackupParams{
		AppVolumeID: volA, DestinationID: destA, Schedule: "0 2 * * *", Prefix: "daily", Retention: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create org-A volume backup: %v", err)
	}
	_ = oA
	_ = appA

	// --- Org-B: its own app chain ---
	uidB := mkUser(t, q, "vbk-b@k.local")
	oB, _ := orgSvc.CreateOrg(ctx, uidB, "OrgB")
	pB, _ := orgSvc.CreateProject(ctx, oB.ID, "PB", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "prod-b")
	appB, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eB.ID, Name: "b", Image: "nginx", Tag: "alpine",
		Domain: "b.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	_, _ = q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: appB.ID, Name: "data", MountPath: "/data"})
	cookieB := loginAs(t, q, "vbk-b@k.local")

	// Org-B's full app chain but with org-A's volume-backup ID, on the FLAT
	// registered route shape (no {volID} segment).
	base := "/orgs/" + i64(oB.ID) + "/projects/" + i64(pB.ID) +
		"/environments/" + i64(eB.ID) + "/apps/" + i64(appB.ID) +
		"/volumes/backups/" + i64(aBackup.ID) + "/delete"
	req := httptest.NewRequest(http.MethodPost, base, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookieB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("SECURITY: cross-tenant volume-backup delete got %d, want 404", rec.Code)
	}
	if _, err := q.GetVolumeBackup(ctx, aBackup.ID); err != nil {
		t.Fatalf("SECURITY: org-A volume backup row was affected by cross-tenant request: %v", err)
	}
}
