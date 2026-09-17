package deploy

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

// A deploy in flight when the process dies leaves its row status='running'
// forever: nothing ever finishes it, so the history shows a deploy that never
// ends and the 2s-polled list spins on it. Startup must reconcile those rows —
// no deploy can be in flight in a process that has just booted.
func TestFailOrphanedDeployments(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "orph@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-orph", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p-orph", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.orph", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	orphan, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if orphan.Status != "running" {
		t.Fatalf("fixture precondition: new deployment status = %q, want running", orphan.Status)
	}
	finished, _ := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"})
	if err := q.FinishDeployment(ctx, db.FinishDeploymentParams{
		ID: finished.ID, Status: "done", ImageTag: "nginx:alpine", ErrorMessage: "", Log: "ok",
	}); err != nil {
		t.Fatalf("finish deployment: %v", err)
	}

	s := NewDBStore(q)
	n, err := s.FailOrphanedDeployments(ctx)
	if err != nil {
		t.Fatalf("FailOrphanedDeployments: %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled %d rows, want 1", n)
	}

	got, err := q.GetDeployment(ctx, orphan.ID)
	if err != nil {
		t.Fatalf("get orphan: %v", err)
	}
	if got.Status != "error" {
		t.Errorf("orphan status = %q, want error", got.Status)
	}
	if got.ErrorMessage == "" {
		t.Error("orphan has no error message explaining the interruption")
	}
	if !got.FinishedAt.Valid {
		t.Error("orphan finished_at not set: the row still reads as in flight")
	}

	// An already-finished deploy must not be touched.
	after, _ := q.GetDeployment(ctx, finished.ID)
	if after.Status != "done" || after.ErrorMessage != "" {
		t.Errorf("finished deploy was rewritten: status=%q err=%q", after.Status, after.ErrorMessage)
	}
}

// A linked database whose stored password cannot be decrypted (rotated or
// missing KRILL_SECRET_KEY) must fail the deploy. Injecting the link anyway
// ships the app a connection string built from a bogus password: the app boots,
// fails to reach its database, and nothing points at the encryption key as the
// cause.
func TestGetApplicationFailsOnUndecryptableDBLinkPassword(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "rot@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-rot", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	p, err := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj-rot", Description: ""})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	e, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web-rot.x", Port: 80, EnvText: "FOO=bar",
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Password encrypted under a key that is no longer the active one.
	secret.Init("old-key")
	encPW := secret.Enc("secretpw")
	secret.Init("new-key")
	defer secret.Init("")

	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-rot2",
		Image: "postgres:16-alpine", Superuser: "appuser", SuperuserPassword: encPW,
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "db", DbName: "appdb", Username: "appuser", Password: encPW,
	})
	if err != nil {
		t.Fatalf("create logical database: %v", err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{
		ApplicationID: app.ID, LogicalDatabaseID: &ldb.ID, VarName: "DB_URL", Scheme: "postgres", Field: "url",
	}); err != nil {
		t.Fatalf("create db link: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err == nil {
		t.Fatalf("expected the deploy to fail, got env %v", got.Env)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
	}
}

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

	// A redis link exercises resolveDBLinkValue's redis branch end-to-end.
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

// TestResolveDBLinkValueErrorClassification exercises resolveDBLinkValue
// directly against a real DB (no querier mock exists at this seam — DBStore.q
// is a concrete *db.Queries, not an interface, so there is nothing to inject
// a synthetic non-ErrNoRows failure into). Instead this drives the two real
// error shapes pgx actually produces: a genuinely-missing row (pgx.ErrNoRows)
// vs. a cancelled context (a stand-in for any transient/non-ErrNoRows
// failure, e.g. pool exhaustion or a deadline). It asserts the classification
// contract: only ErrNoRows collapses to ("", false, nil); everything else
// propagates as a non-nil error.
func TestResolveDBLinkValueErrorClassification(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	s := NewDBStore(q)

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "rc@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-rc", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	p, err := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj-rc", Description: ""})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	e, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-rc",
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
	redisInst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "redis", Name: "cache", AppName: "krill-redis-rc",
		Image: "redis:7-alpine", SuperuserPassword: "rpw",
	})
	if err != nil {
		t.Fatalf("create redis instance: %v", err)
	}

	missingID := int64(987654321)
	pgRow := db.ListDBLinksByApplicationRow{LogicalDatabaseID: &ldb.ID, VarName: "DB_URL", Scheme: "postgres", Field: "url"}
	redisRow := db.ListDBLinksByApplicationRow{InstanceID: &redisInst.ID, VarName: "REDIS_URL", Scheme: "redis", Field: "url"}

	// Success: a real, existing logical database resolves to a value with ok=true, err=nil.
	if val, ok, rerr := s.resolveDBLinkValue(ctx, pgRow); rerr != nil || !ok || val == "" {
		t.Errorf("success case: val=%q ok=%v err=%v, want non-empty val, ok=true, err=nil", val, ok, rerr)
	}

	// Genuinely-deleted target (logical DB): ErrNoRows -> skip, no error.
	missingLDRow := db.ListDBLinksByApplicationRow{LogicalDatabaseID: &missingID, VarName: "DB_URL", Scheme: "postgres", Field: "url"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, missingLDRow); rerr != nil || ok || val != "" {
		t.Errorf("missing logical db: val=%q ok=%v err=%v, want (\"\", false, nil)", val, ok, rerr)
	}

	// Genuinely-deleted target (redis instance): ErrNoRows -> skip, no error.
	missingInstRow := db.ListDBLinksByApplicationRow{InstanceID: &missingID, VarName: "REDIS_URL", Scheme: "redis", Field: "url"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, missingInstRow); rerr != nil || ok || val != "" {
		t.Errorf("missing redis instance: val=%q ok=%v err=%v, want (\"\", false, nil)", val, ok, rerr)
	}

	// Transient failure (stand-in: a cancelled context) must NOT collapse to
	// ("", false, nil) the way ErrNoRows does -- it must propagate as an error
	// so GetApplication fails the deploy instead of silently omitting the link.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if val, ok, rerr := s.resolveDBLinkValue(cancelledCtx, pgRow); rerr == nil || ok || val != "" {
		t.Errorf("cancelled ctx (pg): val=%q ok=%v err=%v, want (\"\", false, non-nil)", val, ok, rerr)
	} else if errors.Is(rerr, pgx.ErrNoRows) {
		t.Errorf("cancelled ctx (pg) misclassified as ErrNoRows: %v", rerr)
	}
	if val, ok, rerr := s.resolveDBLinkValue(cancelledCtx, redisRow); rerr == nil || ok || val != "" {
		t.Errorf("cancelled ctx (redis): val=%q ok=%v err=%v, want (\"\", false, non-nil)", val, ok, rerr)
	} else if errors.Is(rerr, pgx.ErrNoRows) {
		t.Errorf("cancelled ctx (redis) misclassified as ErrNoRows: %v", rerr)
	}
}

// TestResolveDBLinkValueDispatchesByEngine verifies resolveDBLinkValue's
// driver dispatch (Task 5): a minio instance link resolves its S3 fields, a
// dragonfly instance link resolves identically to redis (RESP-compatible),
// and — as a regression check — an existing redis link still resolves
// exactly as it did before the drivers.LinkValue dispatch replaced the
// inline dbLinkFieldValue helper (same user "default", port 6379, no dbname).
func TestResolveDBLinkValueDispatchesByEngine(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	s := NewDBStore(q)

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "disp@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-disp", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	redisInst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "redis", Name: "cache", AppName: "krill-redis-disp",
		Image: "redis:7-alpine", SuperuserPassword: "rpw",
	})
	if err != nil {
		t.Fatalf("create redis instance: %v", err)
	}
	dragonflyInst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "dragonfly", Name: "df", AppName: "krill-dragonfly-disp",
		Image: "docker.dragonflydb.io/dragonflydb/dragonfly:latest", Superuser: "default", SuperuserPassword: "dfpw",
	})
	if err != nil {
		t.Fatalf("create dragonfly instance: %v", err)
	}
	minioInst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "minio", Name: "objs", AppName: "krill-minio-disp",
		Image: "quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z", Superuser: "root-user", SuperuserPassword: "s3secret",
	})
	if err != nil {
		t.Fatalf("create minio instance: %v", err)
	}

	// Regression: redis link resolves identically to the pre-dispatch behavior
	// (user "default", port 6379, no dbname).
	redisRow := db.ListDBLinksByApplicationRow{InstanceID: &redisInst.ID, VarName: "REDIS_URL", Scheme: "redis", Field: "url"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, redisRow); rerr != nil || !ok || val != "redis://default:rpw@krill-redis-disp:6379" {
		t.Errorf("redis url = %q, ok=%v, err=%v", val, ok, rerr)
	}
	redisPassRow := db.ListDBLinksByApplicationRow{InstanceID: &redisInst.ID, VarName: "REDIS_PASS", Scheme: "redis", Field: "password"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, redisPassRow); rerr != nil || !ok || val != "rpw" {
		t.Errorf("redis password = %q, ok=%v, err=%v", val, ok, rerr)
	}

	// DragonFly (RESP-compatible with redis): same shape, different app name.
	dfRow := db.ListDBLinksByApplicationRow{InstanceID: &dragonflyInst.ID, VarName: "DF_URL", Scheme: "redis", Field: "url"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, dfRow); rerr != nil || !ok || val != "redis://default:dfpw@krill-dragonfly-disp:6379" {
		t.Errorf("dragonfly url = %q, ok=%v, err=%v", val, ok, rerr)
	}

	// MinIO: endpoint/access_key/secret_key resolve via the instance's own
	// superuser (= access key) and decrypted password (= secret key).
	epRow := db.ListDBLinksByApplicationRow{InstanceID: &minioInst.ID, VarName: "S3_ENDPOINT", Field: "endpoint"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, epRow); rerr != nil || !ok || val != "http://krill-minio-disp:9000" {
		t.Errorf("minio endpoint = %q, ok=%v, err=%v", val, ok, rerr)
	}
	akRow := db.ListDBLinksByApplicationRow{InstanceID: &minioInst.ID, VarName: "S3_ACCESS_KEY", Field: "access_key"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, akRow); rerr != nil || !ok || val != "root-user" {
		t.Errorf("minio access_key = %q, ok=%v, err=%v", val, ok, rerr)
	}
	skRow := db.ListDBLinksByApplicationRow{InstanceID: &minioInst.ID, VarName: "S3_SECRET_KEY", Field: "secret_key"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, skRow); rerr != nil || !ok || val != "s3secret" {
		t.Errorf("minio secret_key = %q, ok=%v, err=%v", val, ok, rerr)
	}
	// A field the minio driver doesn't expose (e.g. "dbname") -> (_, false, nil),
	// same skip-with-warning contract as ErrNoRows.
	badRow := db.ListDBLinksByApplicationRow{InstanceID: &minioInst.ID, VarName: "BAD", Field: "dbname"}
	if val, ok, rerr := s.resolveDBLinkValue(ctx, badRow); rerr != nil || ok || val != "" {
		t.Errorf("minio unknown field: val=%q ok=%v err=%v, want (\"\", false, nil)", val, ok, rerr)
	}
}

// A registry password that cannot be decrypted must fail the deploy. Silently
// dropping the auth turns a private image into an anonymous pull, which fails
// at the daemon with "manifest unknown" / "unauthorized" and looks like a
// broken image reference rather than a key problem.
func TestGetApplicationFailsOnUndecryptableRegistryPassword(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "reg@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-reg", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj-reg", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})

	secret.Init("old-key")
	encPW := secret.Enc("registry-pw")
	secret.Init("new-key")
	defer secret.Init("")

	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{
		OrganizationID: o.ID, Name: "ghcr", RegistryUrl: "ghcr.io", Username: "me", Password: encPW,
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "ghcr.io/me/app", Tag: "latest",
		Domain: "web.reg", Port: 80, EnvText: "",
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: app.ID, RegistryID: &reg.ID}); err != nil {
		t.Fatalf("set registry: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err == nil {
		t.Fatalf("expected the deploy to fail, got RegistryAuth=%q", got.RegistryAuth)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
	}
}

// A git token that cannot be decrypted must fail the deploy rather than fall
// back to an anonymous clone of a private repository.
func TestGetApplicationFailsOnUndecryptableGitToken(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "gt@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-gt", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj-gt", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})

	secret.Init("old-key")
	encTok := secret.Enc("ghp_secret")
	secret.Init("new-key")
	defer secret.Init("")

	gc, err := q.CreateGitCredential(ctx, db.CreateGitCredentialParams{
		OrganizationID: o.ID, Name: "gh", Host: "github.com", Username: "x-access-token", Token: encTok,
	})
	if err != nil {
		t.Fatalf("create git credential: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.gt", Port: 80, EnvText: "",
		SourceType: "dockerfile", GitUrl: "https://github.com/me/p.git", GitBranch: "main", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := q.SetApplicationGitCredential(ctx, db.SetApplicationGitCredentialParams{ID: app.ID, GitCredentialID: &gc.ID}); err != nil {
		t.Fatalf("set git cred: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err == nil {
		t.Fatalf("expected the deploy to fail, got GitAuth=%+v", got.GitAuth)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
	}
}

// A registry credential must never be handed to a host it wasn't registered
// for: the app's image lives on a different host than the stored registry_url,
// so GetApplication must fail with ErrCredentialHostMismatch instead of
// returning a RegistryAuth blob that would leak the password to that host.
func TestGetApplicationRejectsMismatchedRegistryHost(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "reghost@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-reghost", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj-reghost", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})

	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{
		OrganizationID: o.ID, Name: "ghcr", RegistryUrl: "ghcr.io", Username: "me", Password: secret.Enc("registry-pw"),
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	// Image points at a different host than the registry it's linked to.
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "evil.example/me/app", Tag: "latest",
		Domain: "web.reghost", Port: 80, EnvText: "",
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: app.ID, RegistryID: &reg.ID}); err != nil {
		t.Fatalf("set registry: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err == nil {
		t.Fatalf("expected the deploy to fail, got RegistryAuth=%q", got.RegistryAuth)
	}
	if !errors.Is(err, ErrCredentialHostMismatch) {
		t.Errorf("got err %v, want ErrCredentialHostMismatch", err)
	}
}

// A git credential must never be handed to a host it wasn't registered for:
// the app's git_url points at a different host than the stored credential, so
// GetApplication must fail with ErrCredentialHostMismatch instead of returning
// a GitAuth that would leak the PAT to that host via the askpass helper.
func TestGetApplicationRejectsMismatchedGitCredentialHost(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "gchost@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-gchost", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj-gchost", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})

	gc, err := q.CreateGitCredential(ctx, db.CreateGitCredentialParams{
		OrganizationID: o.ID, Name: "gh", Host: "github.com", Username: "x-access-token", Token: secret.Enc("ghp_secret"),
	})
	if err != nil {
		t.Fatalf("create git credential: %v", err)
	}
	// git_url points at a different host than the credential it's linked to.
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.gchost", Port: 80, EnvText: "",
		SourceType: "dockerfile", GitUrl: "https://evil.example/me/p.git", GitBranch: "main", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := q.SetApplicationGitCredential(ctx, db.SetApplicationGitCredentialParams{ID: app.ID, GitCredentialID: &gc.ID}); err != nil {
		t.Fatalf("set git cred: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err == nil {
		t.Fatalf("expected the deploy to fail, got GitAuth=%+v", got.GitAuth)
	}
	if !errors.Is(err, ErrCredentialHostMismatch) {
		t.Errorf("got err %v, want ErrCredentialHostMismatch", err)
	}
}

// Build secrets that cannot be decrypted must fail the deploy: building without
// them produces an image that is silently missing whatever they fed.
func TestGetApplicationFailsOnUndecryptableBuildSecrets(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "bs@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-bs", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Proj", Slug: "proj-bs", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})

	secret.Init("old-key")
	encSecrets := secret.Enc("NPM_TOKEN=abc")
	secret.Init("new-key")
	defer secret.Init("")

	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.bs", Port: 80, EnvText: "",
		SourceType: "dockerfile", GitUrl: "https://github.com/me/p.git", GitBranch: "main", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := q.UpdateApplicationBuild(ctx, db.UpdateApplicationBuildParams{ID: app.ID, BuildArgs: "A=1", BuildSecrets: encSecrets}); err != nil {
		t.Fatalf("update build: %v", err)
	}

	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err == nil {
		t.Fatalf("expected the deploy to fail, got BuildSecrets=%v", got.BuildSecrets)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
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

// CountRunningDeployments is the global, instance-wide analogue of
// CountRunningDeploymentsByOrg: the self-updater refuses to restart Krill
// while ANY deploy anywhere is running, not just those in one organization.
func TestCountRunningDeployments(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "run-count@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-run-count", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p-run-count", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.run-count", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	if _, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"}); err != nil {
		t.Fatalf("create running deployment 1: %v", err)
	}
	if _, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"}); err != nil {
		t.Fatalf("create running deployment 2: %v", err)
	}
	done, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("create done deployment: %v", err)
	}
	if err := q.FinishDeployment(ctx, db.FinishDeploymentParams{
		ID: done.ID, Status: "done", ImageTag: "nginx:alpine", ErrorMessage: "", Log: "ok",
	}); err != nil {
		t.Fatalf("finish deployment: %v", err)
	}

	got, err := q.CountRunningDeployments(ctx)
	if err != nil {
		t.Fatalf("CountRunningDeployments: %v", err)
	}
	if got != 2 {
		t.Errorf("CountRunningDeployments = %d, want 2", got)
	}
}

func TestGetApplicationContainerLabels(t *testing.T) {
	q := db.New(testutil.NewTestDB(t))
	ctx := context.Background()
	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "lbl@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Acme", Slug: "acme-lbl", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "Shop", Slug: "shop-lbl", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "production", Slug: "production"})
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.lbl", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewDBStore(q).GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	l := got.ContainerLabels
	if l["krill.org"] != "Acme" || l["krill.project"] != "Shop" || l["krill.env"] != "production" ||
		l["krill.app"] != "web" || l["krill.app-id"] != strconv.FormatInt(app.ID, 10) || l["krill.org-id"] != strconv.FormatInt(o.ID, 10) {
		t.Errorf("container labels = %v", l)
	}
}
