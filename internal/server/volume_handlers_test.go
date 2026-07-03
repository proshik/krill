package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestAddVolumeHappyPath(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "vol-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	cookie := loginAs(t, q, "vol-owner@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) +
		"/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)

	rec := postForm(t, h, base+"/volumes", cookie, url.Values{
		"name": {"data"}, "mount_path": {"/data"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("addVolume: got %d, want 303", rec.Code)
	}
	vols, _ := q.ListVolumesByApplication(ctx, app.ID)
	if len(vols) != 1 || vols[0].Name != "data" || vols[0].MountPath != "/data" {
		t.Fatalf("volume not persisted: %+v", vols)
	}

	// Invalid mount path is rejected (no second row).
	postForm(t, h, base+"/volumes", cookie, url.Values{"name": {"bad"}, "mount_path": {"/etc"}})
	if vols, _ := q.ListVolumesByApplication(ctx, app.ID); len(vols) != 1 {
		t.Fatalf("invalid volume should not persist, have %d", len(vols))
	}

	// Duplicate name is rejected.
	postForm(t, h, base+"/volumes", cookie, url.Values{"name": {"data"}, "mount_path": {"/other"}})
	if vols, _ := q.ListVolumesByApplication(ctx, app.ID); len(vols) != 1 {
		t.Fatalf("duplicate name should not persist, have %d", len(vols))
	}

	// Duplicate mount path is rejected (different name, same path).
	postForm(t, h, base+"/volumes", cookie, url.Values{"name": {"data2"}, "mount_path": {"/data"}})
	if vols, _ := q.ListVolumesByApplication(ctx, app.ID); len(vols) != 1 {
		t.Fatalf("duplicate mount path should not persist, have %d", len(vols))
	}
}

func TestVolumeCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()

	uidA := mkUser(t, q, "vol-a@k.local")
	oA, _ := orgSvc.CreateOrg(ctx, uidA, "OrgA")
	pA, _ := orgSvc.CreateProject(ctx, oA.ID, "PA", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "prod-a")
	appA, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eA.ID, Name: "a", Image: "nginx", Tag: "alpine",
		Domain: "a.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	volA, _ := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: appA.ID, Name: "data", MountPath: "/data"})

	uidB := mkUser(t, q, "vol-b@k.local")
	oB, _ := orgSvc.CreateOrg(ctx, uidB, "OrgB")
	pB, _ := orgSvc.CreateProject(ctx, oB.ID, "PB", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "prod-b")
	appB, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eB.ID, Name: "b", Image: "nginx", Tag: "alpine",
		Domain: "b.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	cookieB := loginAs(t, q, "vol-b@k.local")

	// Org-B chain but org-A's volID → must 404 and not delete volA.
	target := "/orgs/" + i64(oB.ID) + "/projects/" + i64(pB.ID) +
		"/environments/" + i64(eB.ID) + "/apps/" + i64(appB.ID) +
		"/volumes/" + i64(volA.ID) + "/delete"
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookieB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("SECURITY: cross-tenant volume delete got %d, want 404", rec.Code)
	}
	if _, err := q.GetVolume(ctx, volA.ID); err != nil {
		t.Fatalf("SECURITY: org-A volume was affected by cross-tenant request: %v", err)
	}
}

func TestVolumeOwnerSetAndEdit(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "vol-own@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	cookie := loginAs(t, q, "vol-own@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) +
		"/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)

	// Add with a bare uid → normalized to uid:uid.
	if rec := postForm(t, h, base+"/volumes", cookie, url.Values{
		"name": {"data"}, "mount_path": {"/data"}, "owner": {"1000"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("addVolume: got %d, want 303", rec.Code)
	}
	vols, _ := q.ListVolumesByApplication(ctx, app.ID)
	if len(vols) != 1 || vols[0].Owner == nil || *vols[0].Owner != "1000:1000" {
		t.Fatalf("owner not normalized/persisted: %+v", vols)
	}

	// Invalid owner is rejected on add (no second row).
	postForm(t, h, base+"/volumes", cookie, url.Values{
		"name": {"bad"}, "mount_path": {"/bad"}, "owner": {"abc"},
	})
	if vs, _ := q.ListVolumesByApplication(ctx, app.ID); len(vs) != 1 {
		t.Fatalf("invalid owner should not persist a volume, have %d", len(vs))
	}

	// Edit the owner via setVolumeOwner.
	if rec := postForm(t, h, base+"/volumes/"+i64(vols[0].ID)+"/owner", cookie, url.Values{
		"owner": {"1000:0"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("setVolumeOwner: got %d, want 303", rec.Code)
	}
	v, _ := q.GetVolume(ctx, vols[0].ID)
	if v.Owner == nil || *v.Owner != "1000:0" {
		t.Fatalf("owner not updated: %+v", v.Owner)
	}

	// Empty owner clears it (NULL).
	postForm(t, h, base+"/volumes/"+i64(vols[0].ID)+"/owner", cookie, url.Values{"owner": {""}})
	v, _ = q.GetVolume(ctx, vols[0].ID)
	if v.Owner != nil {
		t.Fatalf("owner should be cleared, got %v", *v.Owner)
	}
}

func TestSetVolumeOwnerCrossTenant(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uidA := mkUser(t, q, "vo-a@k.local")
	oA, _ := orgSvc.CreateOrg(ctx, uidA, "OrgA")
	pA, _ := orgSvc.CreateProject(ctx, oA.ID, "PA", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "prod-a")
	appA, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eA.ID, Name: "a", Image: "nginx", Tag: "alpine",
		Domain: "a2.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	volA, _ := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: appA.ID, Name: "data", MountPath: "/data"})

	uidB := mkUser(t, q, "vo-b@k.local")
	oB, _ := orgSvc.CreateOrg(ctx, uidB, "OrgB")
	pB, _ := orgSvc.CreateProject(ctx, oB.ID, "PB", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "prod-b")
	appB, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eB.ID, Name: "b", Image: "nginx", Tag: "alpine",
		Domain: "b2.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	cookieB := loginAs(t, q, "vo-b@k.local")

	target := "/orgs/" + i64(oB.ID) + "/projects/" + i64(pB.ID) +
		"/environments/" + i64(eB.ID) + "/apps/" + i64(appB.ID) +
		"/volumes/" + i64(volA.ID) + "/owner"
	if rec := postForm(t, h, target, cookieB, url.Values{"owner": {"1000:0"}}); rec.Code != http.StatusNotFound {
		t.Errorf("SECURITY: cross-tenant setVolumeOwner got %d, want 404", rec.Code)
	}
	if v, _ := q.GetVolume(ctx, volA.ID); v.Owner != nil {
		t.Fatalf("SECURITY: org-A volume owner was modified cross-tenant: %v", *v.Owner)
	}
}
