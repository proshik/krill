package server_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

// bucketFixture is an org with an S3 server, two environments and an app in
// the first. The server's engine is nil, so bucket management on the server
// is a no-op and only Krill's own state is exercised.
type bucketFixture struct {
	h       http.Handler
	q       *db.Queries
	cookie  *http.Cookie
	org     db.Organization
	proj    db.Project
	env     db.Environment
	other   db.Environment
	app     db.Application
	inst    db.DbInstance
	envBase string
	appBase string
	mc      *mcLog
}

func newBucketFixture(t *testing.T, email string) bucketFixture {
	t.Helper()
	mc := &mcLog{}
	h, q, orgSvc, _, _ := buildDeployServer(t, nil, mcEngine{log: mc})
	ctx := context.Background()
	uid := mkUser(t, q, email)
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "prod")
	other, _ := orgSvc.CreateEnvironment(ctx, p.ID, "staging")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "backend", Image: "penpot", Tag: "2", Domain: "b.x", Port: 80,
		EnvText: "FOO=bar", SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "minio", Name: "silo", AppName: "krill-minio-silo-t1",
		Image: "pgsty/silo:RELEASE.2026-10-26T00-00-00Z", Superuser: "rootadmin", SuperuserPassword: secret.Enc("root-pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	envBase := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID)
	return bucketFixture{h: h, q: q, cookie: loginAs(t, q, email), org: o, proj: p, env: e, other: other, app: app, inst: inst,
		envBase: envBase, appBase: envBase + "/apps/" + i64(app.ID), mc: mc}
}

func TestBucketCreateImportAndLink(t *testing.T) {
	f := newBucketFixture(t, "bkt-owner@k.local")
	ctx := context.Background()

	rec := postForm(t, f.h, f.envBase+"/buckets", f.cookie, url.Values{"instance_id": {i64(f.inst.ID)}, "name": {"penpot"}})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("create: %d %q", rec.Code, flashCookieValue(rec))
	}
	buckets, _ := f.q.ListS3BucketsByEnvironment(ctx, f.env.ID)
	if len(buckets) != 1 || buckets[0].Name != "penpot" || !buckets[0].CreatedByKrill || !strings.HasPrefix(buckets[0].AccessKey, "krill-penpot-") {
		t.Fatalf("bucket: %+v", buckets)
	}
	if sk, err := secret.Dec(buckets[0].SecretKey); err != nil || len(sk) < 16 {
		t.Fatalf("a generated secret key must be stored encrypted and strong: %v", err)
	}
	// The same bucket name on the same server is refused.
	if rec := postForm(t, f.h, f.envBase+"/buckets", f.cookie, url.Values{"instance_id": {i64(f.inst.ID)}, "name": {"penpot"}}); !hasErrFlash(rec) {
		t.Fatal("a second claim on the same bucket must be refused")
	}
	// Import keeps the given principal and marks the bucket as not Krill's.
	rec = postForm(t, f.h, f.envBase+"/buckets", f.cookie, url.Values{
		"instance_id": {i64(f.inst.ID)}, "name": {"legacy-assets"}, "mode": {"import"},
		"access_key": {"penpot-assets"}, "secret_key": {"imported-secret"},
	})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("import: %d %q", rec.Code, flashCookieValue(rec))
	}
	buckets, _ = f.q.ListS3BucketsByEnvironment(ctx, f.env.ID)
	if len(buckets) != 2 || buckets[1].AccessKey != "penpot-assets" || buckets[1].CreatedByKrill {
		t.Fatalf("imported: %+v", buckets)
	}
	for _, bad := range []url.Values{
		{"instance_id": {i64(f.inst.ID)}, "name": {"Bad_Name"}},
		{"instance_id": {i64(f.inst.ID)}, "name": {"ok-name"}, "mode": {"import"}, "access_key": {"k"}, "secret_key": {"short"}},
	} {
		if rec := postForm(t, f.h, f.envBase+"/buckets", f.cookie, bad); !hasErrFlash(rec) {
			t.Fatalf("invalid input accepted: %v", bad)
		}
	}

	b := buckets[0]
	// Linking: a field outside the set, a var in env_text and a var another
	// link already injects are refused.
	ok := postForm(t, f.h, f.appBase+"/bucket-links", f.cookie, url.Values{"bucket_id": {i64(b.ID)}, "var_name": {"AWS_SECRET_ACCESS_KEY"}, "field": {"secret_key"}})
	if ok.Code != http.StatusSeeOther || hasErrFlash(ok) {
		t.Fatalf("link: %d %q", ok.Code, flashCookieValue(ok))
	}
	for _, bad := range []url.Values{
		{"bucket_id": {i64(b.ID)}, "var_name": {"X"}, "field": {"password"}},
		{"bucket_id": {i64(b.ID)}, "var_name": {"FOO"}, "field": {"bucket"}},
		{"bucket_id": {i64(b.ID)}, "var_name": {"AWS_SECRET_ACCESS_KEY"}, "field": {"bucket"}},
	} {
		if rec := postForm(t, f.h, f.appBase+"/bucket-links", f.cookie, bad); !hasErrFlash(rec) {
			t.Fatalf("bad link accepted: %v", bad)
		}
	}
	// A DB link may not take a var a bucket link injects.
	redis, _ := f.q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: f.org.ID, Engine: "redis", Name: "cache", AppName: "krill-redis-cache-b1", Image: "redis:7", SuperuserPassword: "pw",
	})
	if rec := postForm(t, f.h, f.appBase+"/db-links", f.cookie, url.Values{"db_ref": {"inst:" + i64(redis.ID)}, "var_name": {"AWS_SECRET_ACCESS_KEY"}, "scheme": {"redis"}, "field": {"password"}}); !flashSays(rec, "already injected by another link") {
		t.Fatal("a DB link must not shadow a bucket link")
	}
	links, _ := f.q.ListBucketLinksByApplication(ctx, f.app.ID)
	if len(links) != 1 {
		t.Fatalf("links: %+v", links)
	}

	// A linked bucket cannot be removed, and its server cannot be deleted.
	if rec := postForm(t, f.h, f.envBase+"/buckets/"+i64(b.ID)+"/delete", f.cookie, url.Values{}); !flashSays(rec, "Apps still use this bucket") {
		t.Fatalf("deleting a linked bucket must be refused as linked, got %q", flashCookieValue(rec))
	}
	if rec := postForm(t, f.h, "/orgs/"+i64(f.org.ID)+"/db-servers/"+i64(f.inst.ID)+"/delete", f.cookie, url.Values{"delete_databases": {"on"}}); !flashSays(rec, "still holds buckets") {
		t.Fatal("deleting an S3 server that still holds buckets must be refused")
	}
	if _, err := f.q.GetDBInstance(ctx, f.inst.ID); err != nil {
		t.Fatalf("server gone: %v", err)
	}

	// Rotation keeps the previous key until it is revoked, and refuses a
	// second rotation in between.
	rec = postForm(t, f.h, f.envBase+"/buckets/"+i64(b.ID)+"/rotate", f.cookie, url.Values{})
	if hasErrFlash(rec) {
		t.Fatalf("rotate: %q", flashCookieValue(rec))
	}
	rotated, _ := f.q.GetS3Bucket(ctx, b.ID)
	if rotated.PrevAccessKey != b.AccessKey || rotated.AccessKey == b.AccessKey || rotated.PrevSecretKey != b.SecretKey {
		t.Fatalf("rotation: %+v", rotated)
	}
	if rec := postForm(t, f.h, f.envBase+"/buckets/"+i64(b.ID)+"/rotate", f.cookie, url.Values{}); !flashSays(rec, "Revoke the previous key") {
		t.Fatal("a second rotation before revoking must be refused")
	}
	postForm(t, f.h, f.envBase+"/buckets/"+i64(b.ID)+"/revoke-previous", f.cookie, url.Values{})
	if revoked, _ := f.q.GetS3Bucket(ctx, b.ID); revoked.PrevAccessKey != "" || revoked.PrevSecretKey != "" {
		t.Fatalf("revoke: %+v", revoked)
	}

	// Unlinked, the bucket can be removed.
	postForm(t, f.h, f.appBase+"/bucket-links/"+i64(links[0].ID)+"/delete", f.cookie, url.Values{})
	if rec := postForm(t, f.h, f.envBase+"/buckets/"+i64(b.ID)+"/delete", f.cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("delete: %q", flashCookieValue(rec))
	}
	if _, err := f.q.GetS3Bucket(ctx, b.ID); err == nil {
		t.Fatal("bucket row still there")
	}
}

// Buckets and bucket links are scoped to their environment and organization:
// another environment's path, another org's member and an outsider get 404.
func TestBucketTenancy(t *testing.T) {
	f := newBucketFixture(t, "bkt-tenant@k.local")
	ctx := context.Background()
	b, err := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: f.inst.ID, EnvironmentID: f.env.ID, Name: "penpot", AccessKey: "krill-penpot-aaaaaa", SecretKey: secret.Enc("sk-1234567890"), CreatedByKrill: true})
	if err != nil {
		t.Fatal(err)
	}
	otherBase := "/orgs/" + i64(f.org.ID) + "/projects/" + i64(f.proj.ID) + "/environments/" + i64(f.other.ID)
	if rec := getPage(t, f.h, otherBase+"/buckets/"+i64(b.ID), f.cookie, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bucket through another environment: %d", rec.Code)
	}
	if rec := postForm(t, f.h, otherBase+"/buckets/"+i64(b.ID)+"/delete", f.cookie, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("delete through another environment: %d", rec.Code)
	}
	// An app of the other environment cannot link this bucket.
	otherApp, _ := f.q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: f.other.ID, Name: "s", Image: "x", Tag: "1", Domain: "s.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if rec := postForm(t, f.h, otherBase+"/apps/"+i64(otherApp.ID)+"/bucket-links", f.cookie, url.Values{"bucket_id": {i64(b.ID)}, "var_name": {"B"}, "field": {"bucket"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-environment link: %d", rec.Code)
	}
	// A bucket on another org's S3 server cannot be created through this org.
	outsider := mkUser(t, f.q, "bkt-outsider@k.local")
	o2, _ := f.q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O2", Slug: "o2-bkt", OwnerID: outsider})
	foreign, _ := f.q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o2.ID, Engine: "minio", Name: "s3", AppName: "krill-minio-o2-t1", Image: "pgsty/silo:x", Superuser: "r", SuperuserPassword: "p"})
	if rec := postForm(t, f.h, f.envBase+"/buckets", f.cookie, url.Values{"instance_id": {i64(foreign.ID)}, "name": {"steal"}}); !hasErrFlash(rec) {
		t.Fatal("a bucket on another organization's server must be refused")
	}
	if n, _ := f.q.CountS3BucketsByInstance(ctx, foreign.ID); n != 0 {
		t.Fatal("foreign server got a bucket")
	}
}

// A member sees the bucket but not its secret key, and cannot change it.
func TestBucketMemberIsReadOnly(t *testing.T) {
	f := newBucketFixture(t, "bkt-admin@k.local")
	ctx := context.Background()
	b, _ := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: f.inst.ID, EnvironmentID: f.env.ID, Name: "penpot", AccessKey: "krill-penpot-bbbbbb", SecretKey: secret.Enc("member-must-not-see"), CreatedByKrill: true})
	memberID := mkUser(t, f.q, "bkt-member@k.local")
	if _, err := f.q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: f.org.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	member := loginAs(t, f.q, "bkt-member@k.local")
	page := getPage(t, f.h, f.envBase+"/buckets/"+i64(b.ID), member, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "penpot") {
		t.Fatalf("member view: %d", page.Code)
	}
	if strings.Contains(page.Body.String(), "member-must-not-see") {
		t.Fatal("the secret key must not reach a member")
	}
	admin := getPage(t, f.h, f.envBase+"/buckets/"+i64(b.ID), f.cookie, nil)
	if !strings.Contains(admin.Body.String(), "member-must-not-see") {
		t.Fatal("the admin view must offer the secret key")
	}
	for _, path := range []string{"/buckets", "/buckets/" + i64(b.ID) + "/rotate", "/buckets/" + i64(b.ID) + "/delete"} {
		if rec := postForm(t, f.h, f.envBase+path, member, url.Values{"instance_id": {i64(f.inst.ID)}, "name": {"x-bucket"}}); rec.Code != http.StatusForbidden {
			t.Fatalf("member POST %s: %d", path, rec.Code)
		}
	}
}

// flashSays reports an error flash containing text.
func flashSays(rec *httptest.ResponseRecorder, text string) bool {
	msg, _ := url.QueryUnescape(flashCookieValue(rec))
	return hasErrFlash(rec) && strings.Contains(msg, text)
}

// A refused deploy says why in the UI instead of a generic queue error.
func TestDeployRefusalSaysWhy(t *testing.T) {
	f := newBucketFixture(t, "dep-refuse@k.local")
	ctx := context.Background()
	if _, err := f.q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: f.app.ID, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	rec := postForm(t, f.h, f.appBase+"/deploy", f.cookie, url.Values{})
	if !flashSays(rec, "already in progress") {
		t.Fatalf("flash: %q", flashCookieValue(rec))
	}
}

// Bucket backup configurations: created with the org's own storage only,
// paused and deleted by admins, and a member can touch none of it.
func TestBucketBackupConfig(t *testing.T) {
	f := newBucketFixture(t, "bkt-backup@k.local")
	ctx := context.Background()
	b, _ := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: f.inst.ID, EnvironmentID: f.env.ID, Name: "penpot", AccessKey: "krill-penpot-dddddd", SecretKey: "x", CreatedByKrill: true})
	dest, err := f.q.CreateDestination(ctx, db.CreateDestinationParams{OrganizationID: f.org.ID, Name: "b2", Endpoint: "https://s3.example.com", Bucket: "bk", Region: "us-east-1", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	outsider := mkUser(t, f.q, "bkt-backup-out@k.local")
	o2, _ := f.q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O2", Slug: "o2-bkb", OwnerID: outsider})
	foreign, _ := f.q.CreateDestination(ctx, db.CreateDestinationParams{OrganizationID: o2.ID, Name: "theirs", Endpoint: "https://s3.example.com", Bucket: "x", Region: "us-east-1", AccessKey: "a", SecretKey: "s"})
	base := f.envBase + "/buckets/" + i64(b.ID)

	form := url.Values{"destination_id": {i64(dest.ID)}, "schedule_preset": {"daily"}, "retention": {"5"}}
	if rec := postForm(t, f.h, base+"/backups", f.cookie, form); hasErrFlash(rec) {
		t.Fatalf("create: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, f.h, base+"/backups", f.cookie, form); !hasErrFlash(rec) {
		t.Fatal("the same storage and prefix twice must be refused")
	}
	if rec := postForm(t, f.h, base+"/backups", f.cookie, url.Values{"destination_id": {i64(foreign.ID)}, "schedule_preset": {"daily"}, "retention": {"5"}}); !hasErrFlash(rec) {
		t.Fatal("another organization's storage must be refused")
	}
	rows, _ := f.q.ListBucketBackupsByBucket(ctx, b.ID)
	if len(rows) != 1 || rows[0].Retention != 5 || !rows[0].Enabled {
		t.Fatalf("rows: %+v", rows)
	}
	bb := base + "/backups/" + i64(rows[0].ID)
	postForm(t, f.h, bb+"/toggle", f.cookie, url.Values{"enabled": {"0"}})
	if r, _ := f.q.GetBucketBackup(ctx, rows[0].ID); r.Enabled {
		t.Fatal("pause did not stick")
	}

	// Without a bucket-backup service (no engine) a restore refuses and
	// creates no bucket.
	if rec := postForm(t, f.h, bb+"/restore", f.cookie, url.Values{"snapshot": {"2026-11-07T10-00-00Z"}, "target": {"penpot-restored"}}); !hasErrFlash(rec) {
		t.Fatal("restore without the service must refuse")
	}
	if n, _ := f.q.CountS3BucketsByInstance(ctx, f.inst.ID); n != 1 {
		t.Fatalf("a refused restore created a bucket: %d", n)
	}

	memberID := mkUser(t, f.q, "bkt-backup-member@k.local")
	f.q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: f.org.ID, UserID: memberID, Role: "member"})
	member := loginAs(t, f.q, "bkt-backup-member@k.local")
	for _, p := range []string{"/backups", "/backups/" + i64(rows[0].ID) + "/run", "/backups/" + i64(rows[0].ID) + "/restore", "/backups/" + i64(rows[0].ID) + "/delete"} {
		if rec := postForm(t, f.h, base+p, member, form); rec.Code != http.StatusForbidden {
			t.Fatalf("member POST %s: %d", p, rec.Code)
		}
	}
	// Deleting the bucket takes its backup configurations with it.
	if rec := postForm(t, f.h, bb+"/delete", f.cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("delete: %q", flashCookieValue(rec))
	}
	if rows, _ := f.q.ListBucketBackupsByBucket(ctx, b.ID); len(rows) != 0 {
		t.Fatalf("left: %+v", rows)
	}
}

// Two rotations racing must not lose the key in use: one wins, the other is
// refused, and the previous key recorded is still the original.
func TestBucketConcurrentRotationKeepsTheOriginalKey(t *testing.T) {
	f := newBucketFixture(t, "bkt-race@k.local")
	ctx := context.Background()
	b, _ := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: f.inst.ID, EnvironmentID: f.env.ID, Name: "penpot", AccessKey: "original-key", SecretKey: secret.Enc("s1-secret"), CreatedByKrill: true})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postForm(t, f.h, f.envBase+"/buckets/"+i64(b.ID)+"/rotate", f.cookie, url.Values{})
		}()
	}
	wg.Wait()
	got, _ := f.q.GetS3Bucket(ctx, b.ID)
	if got.PrevAccessKey != "original-key" || got.AccessKey == "original-key" || !got.AccessKeyGenerated || got.PrevAccessKeyGenerated {
		t.Fatalf("after racing rotations: current %q (generated %v), previous %q (generated %v)", got.AccessKey, got.AccessKeyGenerated, got.PrevAccessKey, got.PrevAccessKeyGenerated)
	}
}

// Ownership of a key is recorded, not guessed from its name: an imported key
// that looks generated stays "not generated", so cleanup never removes it.
func TestImportedKeyIsNeverMarkedGenerated(t *testing.T) {
	f := newBucketFixture(t, "bkt-own@k.local")
	ctx := context.Background()
	rec := postForm(t, f.h, f.envBase+"/buckets", f.cookie, url.Values{
		"instance_id": {i64(f.inst.ID)}, "name": {"assets"}, "mode": {"import"},
		"access_key": {"krill-assets-abcdef"}, "secret_key": {"imported-secret"},
	})
	if hasErrFlash(rec) {
		t.Fatalf("import: %q", flashCookieValue(rec))
	}
	bs, _ := f.q.ListS3BucketsByEnvironment(ctx, f.env.ID)
	if len(bs) != 1 || bs[0].AccessKeyGenerated {
		t.Fatalf("an imported key must not be marked generated: %+v", bs)
	}
	postForm(t, f.h, f.envBase+"/buckets", f.cookie, url.Values{"instance_id": {i64(f.inst.ID)}, "name": {"fresh"}})
	bs, _ = f.q.ListS3BucketsByEnvironment(ctx, f.env.ID)
	if len(bs) != 2 || !bs[1].AccessKeyGenerated {
		t.Fatalf("a created bucket's key is generated: %+v", bs)
	}
}

// Cleanup removes exactly the principals Krill generated, as recorded on the
// row: an imported key that looks generated survives a rotation and a bucket
// delete, and a deleted environment removes only its generated keys.
func TestBucketCleanupRemovesOnlyGeneratedKeys(t *testing.T) {
	f := newBucketFixture(t, "bkt-cleanup@k.local")
	ctx := context.Background()
	const imported = "krill-assets-abcdef"
	if rec := postForm(t, f.h, f.envBase+"/buckets", f.cookie, url.Values{
		"instance_id": {i64(f.inst.ID)}, "name": {"assets"}, "mode": {"import"},
		"access_key": {imported}, "secret_key": {"imported-secret"},
	}); hasErrFlash(rec) {
		t.Fatalf("import: %q", flashCookieValue(rec))
	}
	bs, _ := f.q.ListS3BucketsByEnvironment(ctx, f.env.ID)
	if len(bs) != 1 {
		t.Fatalf("buckets: %+v", bs)
	}
	bb := f.envBase + "/buckets/" + i64(bs[0].ID)
	if rec := postForm(t, f.h, bb+"/rotate", f.cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("rotate: %q", flashCookieValue(rec))
	}
	rotated, _ := f.q.GetS3Bucket(ctx, bs[0].ID)
	if rotated.PrevAccessKey != imported {
		t.Fatalf("rotation must keep the imported key as previous: %+v", rotated)
	}
	if rec := postForm(t, f.h, bb+"/delete", f.cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("delete: %q", flashCookieValue(rec))
	}
	if got := f.mc.removedUsers(); len(got) != 1 || got[0] != rotated.AccessKey {
		t.Fatalf("bucket delete removed %v, want only the generated %q", got, rotated.AccessKey)
	}

	// An environment delete: its generated key goes, its imported one stays.
	otherBase := "/orgs/" + i64(f.org.ID) + "/projects/" + i64(f.proj.ID) + "/environments/" + i64(f.other.ID)
	if rec := postForm(t, f.h, otherBase+"/buckets", f.cookie, url.Values{"instance_id": {i64(f.inst.ID)}, "name": {"fresh"}}); hasErrFlash(rec) {
		t.Fatalf("create: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, f.h, otherBase+"/buckets", f.cookie, url.Values{
		"instance_id": {i64(f.inst.ID)}, "name": {"media"}, "mode": {"import"},
		"access_key": {"krill-media-abcdef"}, "secret_key": {"imported-secret"},
	}); hasErrFlash(rec) {
		t.Fatalf("import: %q", flashCookieValue(rec))
	}
	created, _ := f.q.ListS3BucketsByEnvironment(ctx, f.other.ID)
	var generated string
	for _, b := range created {
		if b.Name == "fresh" {
			generated = b.AccessKey
		}
	}
	if generated == "" {
		t.Fatalf("buckets in the other environment: %+v", created)
	}
	before := len(f.mc.removedUsers())
	if rec := postForm(t, f.h, otherBase+"/delete", f.cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("environment delete: %q", flashCookieValue(rec))
	}
	if got := f.mc.removedUsers()[before:]; len(got) != 1 || got[0] != generated {
		t.Fatalf("environment delete removed %v, want only the generated %q", got, generated)
	}
}

// Storage is compared by where it writes, not by the destination's id, and a
// storage referenced by a bucket backup cannot be deleted from under it.
func TestBucketBackupStorageIdentity(t *testing.T) {
	f := newBucketFixture(t, "bkt-store@k.local")
	ctx := context.Background()
	b, _ := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: f.inst.ID, EnvironmentID: f.env.ID, Name: "penpot", AccessKey: "k", SecretKey: "x", CreatedByKrill: true})
	d1, _ := f.q.CreateDestination(ctx, db.CreateDestinationParams{OrganizationID: f.org.ID, Name: "b2", Endpoint: "https://s3.example.com", Bucket: "bk", Region: "us-east-1", AccessKey: "a", SecretKey: "s"})
	d2, _ := f.q.CreateDestination(ctx, db.CreateDestinationParams{OrganizationID: f.org.ID, Name: "b2-again", Endpoint: "HTTPS://s3.example.com/", Bucket: "bk", Region: "us-east-1", AccessKey: "a", SecretKey: "s"})
	base := f.envBase + "/buckets/" + i64(b.ID) + "/backups"
	if rec := postForm(t, f.h, base, f.cookie, url.Values{"destination_id": {i64(d1.ID)}, "schedule_preset": {"daily"}, "retention": {"3"}}); hasErrFlash(rec) {
		t.Fatalf("first: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, f.h, base, f.cookie, url.Values{"destination_id": {i64(d2.ID)}, "schedule_preset": {"daily"}, "retention": {"1"}}); !hasErrFlash(rec) {
		t.Fatal("a second destination naming the same storage must be refused")
	}
	rec := postForm(t, f.h, "/orgs/"+i64(f.org.ID)+"/destinations/"+i64(d1.ID)+"/delete", f.cookie, url.Values{})
	if msg, _ := url.QueryUnescape(flashCookieValue(rec)); !hasErrFlash(rec) || strings.Contains(msg, "violates") {
		t.Fatalf("deleting storage in use must be refused cleanly, got %q", msg)
	}
}

// The metrics token variable may not take a name a bucket link injects.
func TestMetricsTokenEnvRefusesBucketLinkVar(t *testing.T) {
	f := newBucketFixture(t, "bkt-metrics@k.local")
	ctx := context.Background()
	b, _ := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: f.inst.ID, EnvironmentID: f.env.ID, Name: "penpot", AccessKey: "k", SecretKey: "x", CreatedByKrill: true})
	f.q.CreateBucketLink(ctx, db.CreateBucketLinkParams{ApplicationID: f.app.ID, BucketID: b.ID, VarName: "AWS_SECRET_ACCESS_KEY", Field: "secret_key"})
	rec := postForm(t, f.h, f.appBase+"/metrics/token-env", f.cookie, url.Values{"name": {"AWS_SECRET_ACCESS_KEY"}})
	if !hasErrFlash(rec) {
		t.Fatal("the metrics token must not shadow a bucket link")
	}
}

// mcEngine answers the mc commands bucket management runs like a real S3
// server with an empty bucket would: a probe object does not exist until it
// is written, and reads back what was written.
type mcEngine struct {
	noopEngine
	log *mcLog
}

// mcLog records the mc commands run, for asserting which principals went.
type mcLog struct {
	mu   sync.Mutex
	cmds []string
}

// removedUsers lists the principals removed with `mc admin user remove`.
func (l *mcLog) removedUsers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, c := range l.cmds {
		if _, key, ok := strings.Cut(c, "admin user remove k "); ok {
			out = append(out, key)
		}
	}
	return out
}

func (e mcEngine) Exec(_ context.Context, _ string, cmd []string, _ []string, _ io.Reader, out io.Writer) error {
	joined := strings.Join(cmd, " ")
	if e.log != nil {
		e.log.mu.Lock()
		e.log.cmds = append(e.log.cmds, joined)
		e.log.mu.Unlock()
	}
	switch {
	case strings.Contains(joined, " stat ") && strings.Contains(joined, ".krill-import-check-"):
		io.WriteString(out, `{"status":"error","error":{"message":"Unable to stat","cause":{"message":"Object does not exist"}}}`+"\n")
		return errors.New("exited 1")
	case len(cmd) >= 2 && cmd[0] == "mc" && cmd[1] == "cat":
		io.WriteString(out, "krill")
	default:
		io.WriteString(out, `{"status":"success"}`+"\n")
	}
	return nil
}
