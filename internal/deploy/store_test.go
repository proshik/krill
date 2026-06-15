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

func TestStrDeref(t *testing.T) {
	if got := strDeref(nil); got != "" {
		t.Errorf("strDeref(nil) = %q, want \"\"", got)
	}
	if got := strDeref(sp("x")); got != "x" {
		t.Errorf("strDeref(\"x\") = %q, want \"x\"", got)
	}
}

func TestBuildDBURL(t *testing.T) {
	cases := []struct{ engine, scheme, user, pass, host, dbn, want string }{
		{"postgres", "postgresql", "u", "p", "krill-postgres-x", "app", "postgresql://u:p@krill-postgres-x:5432/app"},
		{"postgres", "postgres", "u", "p", "krill-postgres-x", "app", "postgres://u:p@krill-postgres-x:5432/app"},
		{"redis", "redis", "", "secret", "krill-redis-y", "", "redis://default:secret@krill-redis-y:6379"},
	}
	for _, c := range cases {
		if got := buildDBURL(c.engine, c.scheme, c.user, c.pass, c.host, c.dbn); got != c.want {
			t.Errorf("buildDBURL(%q,%q,...) = %q, want %q", c.engine, c.scheme, got, c.want)
		}
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
	pg, err := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID:    e.ID,
		Name:             "db",
		AppName:          "krill-postgres-db",
		DatabaseName:     "appdb",
		DatabaseUser:     "appuser",
		DatabasePassword: "secretpw",
		Image:            "postgres:16-alpine",
	})
	if err != nil {
		t.Fatalf("create postgres: %v", err)
	}

	// Link DB_URL -> pg with scheme "postgres" (overrides env_text DB_URL).
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{
		ApplicationID: app.ID, Engine: "postgres", DbID: pg.ID, VarName: "DB_URL", Scheme: "postgres",
	}); err != nil {
		t.Fatalf("create db link: %v", err)
	}

	// A redis link exercises resolveDBLinkURL's redis branch end-to-end.
	rd, err := q.CreateRedis(ctx, db.CreateRedisParams{
		EnvironmentID: e.ID, Name: "cache", AppName: "krill-redis-cache", Password: "rpw", Image: "redis:7-alpine",
	})
	if err != nil {
		t.Fatalf("create redis: %v", err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{
		ApplicationID: app.ID, Engine: "redis", DbID: rd.ID, VarName: "REDIS_URL", Scheme: "redis",
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

	// Delete the DB -> link skipped, env_text value restored, not fatal.
	if err := q.DeletePostgres(ctx, pg.ID); err != nil {
		t.Fatalf("delete postgres: %v", err)
	}
	got2, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("after db delete: %v", err)
	}
	if got2.Env["DB_URL"] != "manual" {
		t.Errorf("after DB delete expected env_text 'manual', got %q", got2.Env["DB_URL"])
	}
}
