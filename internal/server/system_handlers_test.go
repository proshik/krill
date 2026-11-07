package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// Systems group an environment's apps without owning anything: an app is in
// at most one, moving or removing changes no app, and deleting a system
// leaves its apps and their resources in place.
func TestSystemsGroupAppsWithoutOwningThem(t *testing.T) {
	f := newBucketFixture(t, "sys-owner@k.local")
	ctx := context.Background()
	front, _ := f.q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: f.env.ID, Name: "frontend", Image: "penpot", Tag: "2", Domain: "f.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if rec := postForm(t, f.h, f.envBase+"/systems", f.cookie, url.Values{"name": {"Penpot"}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("create: %d %q", rec.Code, flashCookieValue(rec))
	}
	if rec := postForm(t, f.h, f.envBase+"/systems", f.cookie, url.Values{"name": {"Penpot"}}); !flashSays(rec, "already has a system") {
		t.Fatal("a duplicate name must be refused")
	}
	if rec := postForm(t, f.h, f.envBase+"/systems", f.cookie, url.Values{"name": {"  "}}); !hasErrFlash(rec) {
		t.Fatal("an empty name must be refused")
	}
	postForm(t, f.h, f.envBase+"/systems", f.cookie, url.Values{"name": {"Other"}})
	systems, _ := f.q.ListSystemsByEnvironment(ctx, f.env.ID)
	if len(systems) != 2 {
		t.Fatalf("systems: %+v", systems)
	}
	other, penpot := systems[0], systems[1] // ordered by name
	sysURL := func(s db.System) string { return f.envBase + "/systems/" + i64(s.ID) }

	postForm(t, f.h, sysURL(penpot)+"/apps", f.cookie, url.Values{"app_id": {i64(f.app.ID)}})
	postForm(t, f.h, sysURL(penpot)+"/apps", f.cookie, url.Values{"app_id": {i64(front.ID)}})
	// Moving an app into another system takes it out of the first one.
	postForm(t, f.h, sysURL(other)+"/apps", f.cookie, url.Values{"app_id": {i64(front.ID)}})
	members, _ := f.q.ListSystemMembersByEnvironment(ctx, f.env.ID)
	got := map[int64]int64{}
	for _, m := range members {
		got[m.ApplicationID] = m.SystemID
	}
	if len(got) != 2 || got[f.app.ID] != penpot.ID || got[front.ID] != other.ID {
		t.Fatalf("membership: %v", got)
	}

	// The environment page renders each system with its apps.
	page := getPage(t, f.h, f.envBase, f.cookie, nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "data-sys") || !strings.Contains(body, `data-sys-app="`+i64(f.app.ID)+`"`) {
		t.Fatalf("environment page: %d", page.Code)
	}

	postForm(t, f.h, sysURL(other)+"/rename", f.cookie, url.Values{"name": {"Frontends"}})
	if s, _ := f.q.GetSystem(ctx, other.ID); s.Name != "Frontends" {
		t.Fatalf("rename: %q", s.Name)
	}
	// Removing an app that is not in the system is a 404, not a silent move.
	if rec := postForm(t, f.h, sysURL(penpot)+"/apps/"+i64(front.ID)+"/remove", f.cookie, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("remove a non-member: %d", rec.Code)
	}
	postForm(t, f.h, sysURL(other)+"/apps/"+i64(front.ID)+"/remove", f.cookie, url.Values{})

	// Deleting a system ungroups its apps and touches nothing else.
	b, _ := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: f.inst.ID, EnvironmentID: f.env.ID, Name: "penpot", AccessKey: "krill-penpot-cccccc", SecretKey: "x", CreatedByKrill: true})
	f.q.CreateBucketLink(ctx, db.CreateBucketLinkParams{ApplicationID: f.app.ID, BucketID: b.ID, VarName: "S3_BUCKET", Field: "bucket"})
	if rec := postForm(t, f.h, sysURL(penpot)+"/delete", f.cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("delete: %q", flashCookieValue(rec))
	}
	if _, err := f.q.GetApplication(ctx, f.app.ID); err != nil {
		t.Fatalf("app gone with its system: %v", err)
	}
	if links, _ := f.q.ListBucketLinksByApplication(ctx, f.app.ID); len(links) != 1 {
		t.Fatal("deleting a system must not touch its apps' links")
	}
	if _, err := f.q.GetS3Bucket(ctx, b.ID); err != nil {
		t.Fatal("deleting a system must not touch a bucket")
	}
	if members, _ := f.q.ListSystemMembersByEnvironment(ctx, f.env.ID); len(members) != 0 {
		t.Fatalf("members left: %+v", members)
	}
}

// A system is reachable only through its own environment, an app of another
// environment cannot join it, and members cannot change systems.
func TestSystemTenancyAndRBAC(t *testing.T) {
	f := newBucketFixture(t, "sys-tenant@k.local")
	ctx := context.Background()
	sy, err := f.q.CreateSystem(ctx, db.CreateSystemParams{EnvironmentID: f.env.ID, Name: "Penpot"})
	if err != nil {
		t.Fatal(err)
	}
	otherBase := "/orgs/" + i64(f.org.ID) + "/projects/" + i64(f.proj.ID) + "/environments/" + i64(f.other.ID)
	if rec := postForm(t, f.h, otherBase+"/systems/"+i64(sy.ID)+"/delete", f.cookie, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("system through another environment: %d", rec.Code)
	}
	stray, _ := f.q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: f.other.ID, Name: "stray", Image: "x", Tag: "1", Domain: "s.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if rec := postForm(t, f.h, f.envBase+"/systems/"+i64(sy.ID)+"/apps", f.cookie, url.Values{"app_id": {i64(stray.ID)}}); rec.Code != http.StatusNotFound {
		t.Fatalf("an app of another environment joined: %d", rec.Code)
	}
	memberID := mkUser(t, f.q, "sys-member@k.local")
	if _, err := f.q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: f.org.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	member := loginAs(t, f.q, "sys-member@k.local")
	for _, path := range []string{"/systems", "/systems/" + i64(sy.ID) + "/rename", "/systems/" + i64(sy.ID) + "/apps", "/systems/" + i64(sy.ID) + "/delete"} {
		if rec := postForm(t, f.h, f.envBase+path, member, url.Values{"name": {"x"}, "app_id": {i64(f.app.ID)}}); rec.Code != http.StatusForbidden {
			t.Fatalf("member POST %s: %d", path, rec.Code)
		}
	}
	// Members see the systems view, without its management controls.
	f.q.SetAppSystem(ctx, db.SetAppSystemParams{ApplicationID: f.app.ID, SystemID: sy.ID})
	page := getPage(t, f.h, f.envBase, member, nil)
	if body := page.Body.String(); !strings.Contains(body, "data-sys") || strings.Contains(body, "/systems/"+i64(sy.ID)+"/delete") {
		t.Fatal("members see systems but not their management")
	}
}
