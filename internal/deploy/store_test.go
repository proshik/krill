package deploy

import (
	"context"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func sp(s string) *string { return &s }
func ip(v int32) *int32   { return &v }

func TestBuildHealthcheckNil(t *testing.T) {
	// No command at all => healthcheck disabled.
	if hc := buildHealthcheck(db.Application{}); hc != nil {
		t.Fatalf("buildHealthcheck(empty) = %+v, want nil", hc)
	}
	// Whitespace-only command also counts as empty.
	if hc := buildHealthcheck(db.Application{HealthcheckCmd: sp("   ")}); hc != nil {
		t.Fatalf("buildHealthcheck(blank cmd) = %+v, want nil", hc)
	}
}

func TestBuildHealthcheckFull(t *testing.T) {
	a := db.Application{
		HealthcheckCmd:         sp("curl -f http://localhost/ || exit 1"),
		HealthcheckInterval:    sp("30s"),
		HealthcheckTimeout:     sp("5s"),
		HealthcheckStartPeriod: sp("10s"),
		HealthcheckRetries:     ip(3),
	}
	hc := buildHealthcheck(a)
	if hc == nil {
		t.Fatal("buildHealthcheck returned nil for configured healthcheck")
	}
	wantTest := []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"}
	if len(hc.Test) != 2 || hc.Test[0] != wantTest[0] || hc.Test[1] != wantTest[1] {
		t.Errorf("Test = %v, want %v", hc.Test, wantTest)
	}
	if hc.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", hc.Interval)
	}
	if hc.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", hc.Timeout)
	}
	if hc.StartPeriod != 10*time.Second {
		t.Errorf("StartPeriod = %v, want 10s", hc.StartPeriod)
	}
	if hc.Retries != 3 {
		t.Errorf("Retries = %d, want 3", hc.Retries)
	}
}

func TestBuildHealthcheckInvalidDurations(t *testing.T) {
	// Command set but durations invalid/empty/retries nil: zero values remain.
	a := db.Application{
		HealthcheckCmd:         sp("true"),
		HealthcheckInterval:    sp("not-a-duration"),
		HealthcheckTimeout:     sp(""),
		HealthcheckStartPeriod: nil,
		HealthcheckRetries:     nil,
	}
	hc := buildHealthcheck(a)
	if hc == nil {
		t.Fatal("buildHealthcheck returned nil for configured healthcheck")
	}
	if hc.Interval != 0 {
		t.Errorf("Interval = %v, want 0 (invalid duration)", hc.Interval)
	}
	if hc.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (empty duration)", hc.Timeout)
	}
	if hc.StartPeriod != 0 {
		t.Errorf("StartPeriod = %v, want 0 (nil)", hc.StartPeriod)
	}
	if hc.Retries != 0 {
		t.Errorf("Retries = %d, want 0 (nil)", hc.Retries)
	}
}

func TestDBLinkFieldValue(t *testing.T) {
	// postgres source
	for field, want := range map[string]string{
		"url":      "postgresql://u:p@h:5432/d",
		"":         "postgresql://u:p@h:5432/d", // empty defaults to url
		"password": "p",
		"host":     "h",
		"port":     "5432",
		"user":     "u",
		"dbname":   "d",
	} {
		if got := dbLinkFieldValue(field, "u", "p", "h", "5432", "d", "postgresql"); got != want {
			t.Errorf("pg field %q = %q, want %q", field, got, want)
		}
	}
	// redis source (no dbname): url must omit the trailing /db
	if got := dbLinkFieldValue("url", "default", "p", "h", "6379", "", "redis"); got != "redis://default:p@h:6379" {
		t.Errorf("redis url = %q", got)
	}
	if got := dbLinkFieldValue("password", "default", "p", "h", "6379", "", "redis"); got != "p" {
		t.Errorf("redis password = %q", got)
	}
}

func TestStrDeref(t *testing.T) {
	if got := strDeref(nil); got != "" {
		t.Errorf("strDeref(nil) = %q, want \"\"", got)
	}
	if got := strDeref(sp("x")); got != "x" {
		t.Errorf("strDeref(\"x\") = %q, want \"x\"", got)
	}
}

func TestGetApplicationInjectsDBLinks(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "u@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	p, err := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj", Description: ""})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	e, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, EnvText: "FOO=bar\nDB_URL=manual",
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-db",
		Image: "postgres:16-alpine", Superuser: "appuser", SuperuserPassword: "secretpw",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "db", DbName: "appdb", Username: "appuser", Password: "secretpw",
	})
	if err != nil {
		t.Fatalf("create logical database: %v", err)
	}

	// Link DB_URL -> pg with scheme "postgres" (overrides env_text DB_URL).
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{
		ApplicationID: app.ID, LogicalDatabaseID: &ldb.ID, VarName: "DB_URL", Scheme: "postgres", Field: "url",
	}); err != nil {
		t.Fatalf("create db link: %v", err)
	}

	// A redis link exercises resolveDBLinkURL's redis branch end-to-end.
	redisInst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "redis", Name: "cache", AppName: "krill-redis-cache",
		Image: "redis:7-alpine", SuperuserPassword: "rpw",
	})
	if err != nil {
		t.Fatalf("create redis instance: %v", err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{
		ApplicationID: app.ID, InstanceID: &redisInst.ID, VarName: "REDIS_URL", Scheme: "redis", Field: "url",
	}); err != nil {
		t.Fatalf("create redis db link: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("env_text not preserved: %q", got.Env["FOO"])
	}
	// secret.Dec is identity without KRILL_SECRET_KEY -> plaintext password verbatim.
	want := "postgres://appuser:secretpw@krill-postgres-db:5432/appdb"
	if got.Env["DB_URL"] != want {
		t.Errorf("link did not override env_text: got %q want %q", got.Env["DB_URL"], want)
	}
	if got.Env["REDIS_URL"] != "redis://default:rpw@krill-redis-cache:6379" {
		t.Errorf("redis link not injected: got %q", got.Env["REDIS_URL"])
	}

	// Delete the logical DB -> the link cascades away (FK ON DELETE CASCADE),
	// env_text value restored, not fatal.
	if err := q.DeleteLogicalDatabase(ctx, ldb.ID); err != nil {
		t.Fatalf("delete logical database: %v", err)
	}
	got2, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("after db delete: %v", err)
	}
	if got2.Env["DB_URL"] != "manual" {
		t.Errorf("after DB delete expected env_text 'manual', got %q", got2.Env["DB_URL"])
	}
}

func TestGetApplicationInjectsBuildCredentials(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "bc@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})

	gc, err := q.CreateGitCredential(ctx, db.CreateGitCredentialParams{
		OrganizationID: o.ID, Name: "gh", Host: "github.com", Username: "x-access-token", Token: "ghp_x",
	})
	if err != nil {
		t.Fatalf("create git credential: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.bc", Port: 80, EnvText: "",
		SourceType: "dockerfile", GitUrl: "https://github.com/me/p.git", GitBranch: "main", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := q.SetApplicationGitCredential(ctx, db.SetApplicationGitCredentialParams{ID: app.ID, GitCredentialID: &gc.ID}); err != nil {
		t.Fatalf("set git cred: %v", err)
	}
	// secret.Dec is identity without KRILL_SECRET_KEY, so store plaintext here.
	if err := q.UpdateApplicationBuild(ctx, db.UpdateApplicationBuildParams{ID: app.ID, BuildArgs: "A=1\nB=2", BuildSecrets: "S=x"}); err != nil {
		t.Fatalf("update build: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if got.GitAuth == nil || got.GitAuth.Username != "x-access-token" || got.GitAuth.Token != "ghp_x" {
		t.Errorf("GitAuth = %+v", got.GitAuth)
	}
	if got.BuildArgs["A"] != "1" || got.BuildArgs["B"] != "2" {
		t.Errorf("BuildArgs = %v", got.BuildArgs)
	}
	if got.BuildSecrets["S"] != "x" {
		t.Errorf("BuildSecrets = %v", got.BuildSecrets)
	}

	// An image-source app must ignore build credentials/args/secrets.
	img, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "img", Image: "nginx", Tag: "alpine",
		Domain: "img.bc", Port: 80, EnvText: "",
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	_ = q.UpdateApplicationBuild(ctx, db.UpdateApplicationBuildParams{ID: img.ID, BuildArgs: "A=1", BuildSecrets: "S=x"})
	gotImg, err := NewDBStore(q).GetApplication(ctx, img.ID)
	if err != nil {
		t.Fatalf("GetApplication img: %v", err)
	}
	if gotImg.GitAuth != nil || len(gotImg.BuildArgs) != 0 || len(gotImg.BuildSecrets) != 0 {
		t.Errorf("image app should ignore build fields: auth=%v args=%v secrets=%v", gotImg.GitAuth, gotImg.BuildArgs, gotImg.BuildSecrets)
	}
}
