//go:build integration

package envclone

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/testutil"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Real psql executes against an owned container. The state lookup also models
// RemoteClientProvider needing the ordinary pool while an exec is dispatched.
type coordinatorPGEngine struct {
	docker.Engine
	container      string
	state          *pgxpool.Pool
	creates, drops int
	afterProvision func() error
}

func (e *coordinatorPGEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (e *coordinatorPGEngine) Exec(ctx context.Context, name string, args, env []string, in io.Reader, out io.Writer) error {
	if name != "owned-coordinator-pg" {
		return errors.New("non-owned exec refused")
	}
	if _, err := e.state.Exec(ctx, "SELECT 1"); err != nil {
		return err
	}
	script, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	if strings.Contains(string(script), "CREATE DATABASE") {
		e.creates++
	}
	if strings.Contains(string(script), "DROP DATABASE") {
		e.drops++
	}
	argv := []string{"exec", "-i"}
	for _, v := range env {
		argv = append(argv, "-e", v)
	}
	argv = append(argv, e.container)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "docker", argv...)
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Stdout = out
	err = cmd.Run() // SQL/password-bearing stderr is intentionally discarded.
	if err == nil && strings.Contains(string(script), "CREATE DATABASE") && e.afterProvision != nil {
		return e.afterProvision()
	}
	return err
}
func coordinatorPGFixture(t *testing.T, private bool) (*Service, Request, *pgxpool.Pool, *coordinatorPGEngine) {
	t.Helper()
	ctx := t.Context()
	base := testutil.NewTestDB(t)
	cfg := base.Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := cloneFixture(t, pool, "coordinator")
	pg, err := postgres.Run(ctx, "postgres:16-alpine", postgres.WithDatabase("postgres"), postgres.WithUsername("fixture"), postgres.WithPassword("fixture-password"), testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2)))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, pg)
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	target, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(target.Close)
	for _, sql := range []string{"CREATE USER source_owner PASSWORD 'fixture_source_password'", "CREATE DATABASE source_data OWNER source_owner"} {
		if _, err := target.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if private {
		if _, err := target.Exec(ctx, "REVOKE CONNECT ON DATABASE source_data FROM PUBLIC"); err != nil {
			t.Fatal(err)
		}
	}
	sourceCfg := target.Config().Copy()
	sourceCfg.ConnConfig.Database = "source_data"
	source, err := pgxpool.NewWithConfig(ctx, sourceCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Exec(ctx, "CREATE TABLE sentinel(value text); INSERT INTO sentinel VALUES ('production')"); err != nil {
		t.Fatal(err)
	}
	q := db.New(pool)
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: r.OrgID, Engine: "postgres", Name: "pg", AppName: "owned-coordinator-pg", Image: "postgres:16-alpine", Superuser: "fixture", SuperuserPassword: "fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: inst.ID, EnvironmentID: r.SourceID, Name: "data", DbName: "source_data", Username: "source_owner", Password: "fixture_source_password"}); err != nil {
		t.Fatal(err)
	}
	engine := &coordinatorPGEngine{container: pg.GetContainerID(), state: pool}
	svc := New(pool, dbservice.New(engine, dbservice.NewDBStore(q), nil, "fixture"), engine, "fixture.test")
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	return svc, r, target, engine
}

func TestCoordinatorRejectsLegacyPublicConnect(t *testing.T) {
	svc, r, target, engine := coordinatorPGFixture(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	id, err := svc.Clone(ctx, r)
	if err == nil || id != 0 {
		t.Fatalf("legacy PUBLIC CONNECT accepted: target=%d", id)
	}
	if engine.creates != 0 {
		t.Fatal("unsafe source unnecessarily provisioned")
	}
	var connect bool
	if err := target.QueryRow(ctx, "SELECT has_database_privilege('source_owner','source_data','CONNECT')").Scan(&connect); err != nil || !connect {
		t.Fatal("source owner's ACL changed")
	}
	var public bool
	if err := target.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_database d,aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) a WHERE d.datname='source_data' AND a.grantee=0 AND a.privilege_type='CONNECT')").Scan(&public); err != nil || !public {
		t.Fatal("source PUBLIC ACL changed")
	}
	var roles int
	if err := target.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'data_%'").Scan(&roles); err != nil || roles != 0 {
		t.Fatal("new role leaked")
	}
	sourceCfg := target.Config().Copy()
	sourceCfg.ConnConfig.Database = "source_data"
	source, err := pgxpool.NewWithConfig(ctx, sourceCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var sentinel string
	if err := source.QueryRow(ctx, "SELECT value FROM sentinel").Scan(&sentinel); err != nil || sentinel != "production" {
		t.Fatal("legacy source data changed")
	}
	envs, err := db.New(svc.pool).ListEnvironments(ctx, r.ProjectID)
	if err != nil || len(envs) != 1 {
		t.Fatal("unsafe clone published")
	}
}

// Only the pgx transport boundary is faulty: Commit really commits first.
// No journal, provisioning, publication or business-state method is mocked.
type lostCommitDB struct {
	journalDB
	commits           int
	lost, unavailable bool
	writesAfterLoss   int
}
type lostCommitTx struct {
	pgx.Tx
	db *lostCommitDB
}

func (d *lostCommitDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.journalDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &lostCommitTx{Tx: tx, db: d}, nil
}
func (tx *lostCommitTx) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	if err != nil {
		return err
	}
	tx.db.commits++
	if tx.db.commits == 2 {
		tx.db.lost = true
		return io.ErrUnexpectedEOF
	}
	return nil
}

type unavailableRow struct{}

func (unavailableRow) Scan(...any) error { return errors.New("injected outcome read unavailable") }
func (d *lostCommitDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if d.lost && d.unavailable {
		return unavailableRow{}
	}
	return d.journalDB.QueryRow(ctx, sql, args...)
}
func (d *lostCommitDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if d.lost {
		d.writesAfterLoss++
	}
	return d.journalDB.Exec(ctx, sql, args...)
}

func TestCoordinatorLostCommitReplyPreservesPublishedPG(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "resolved", true: "outcome-unavailable"}[unavailable], func(t *testing.T) {
			svc, r, target, engine := coordinatorPGFixture(t, true)
			boundary := &lostCommitDB{journalDB: svc.pool, unavailable: unavailable}
			svc.journal = NewJournal(boundary)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			id, err := svc.Clone(ctx, r)
			if !boundary.lost {
				t.Fatal("COMMIT boundary not reached")
			}
			if unavailable {
				if !errors.Is(err, ErrOutcomeUnknown) || id != 0 {
					t.Fatalf("unknown outcome not preserved: id=%d err=%v", id, err)
				}
			} else if err != nil || id == 0 {
				t.Fatalf("committed outcome not resolved: %v", err)
			}
			if engine.drops != 0 || boundary.writesAfterLoss != 0 {
				t.Fatal("ambiguous commit caused compensation")
			}
			op, err := db.New(svc.pool).GetEnvironmentClone(ctx, r.Key)
			if err != nil || op.State != "succeeded" || op.TargetID == nil {
				t.Fatal("durable success missing")
			}
			rows, err := db.New(svc.pool).ListLogicalDatabasesByEnvironment(ctx, *op.TargetID)
			if err != nil || len(rows) != 1 {
				t.Fatal("published mapping missing")
			}
			var exists, allowed bool
			if err := target.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_database WHERE datname=$1),has_database_privilege($2,'source_data','CONNECT')", rows[0].DbName, rows[0].Username).Scan(&exists, &allowed); err != nil || !exists || allowed {
				t.Fatal("published PG removed or source connect permitted")
			}
		})
	}
}

// An ACL change after preflight must still prevent publication. Only the owned
// test server is changed; compensation must preserve its data and the new ACL.
func TestCoordinatorRejectsAccessGrantedDuringProvision(t *testing.T) {
	svc, r, target, engine := coordinatorPGFixture(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	engine.afterProvision = func() error {
		_, err := target.Exec(ctx, "GRANT CONNECT ON DATABASE source_data TO PUBLIC")
		return err
	}
	id, err := svc.Clone(ctx, r)
	if !errors.Is(err, dbservice.ErrCloneIsolation) || id != 0 {
		t.Fatalf("effective access not refused: id=%d err=%v", id, err)
	}
	if engine.creates != 1 || engine.drops != 1 {
		t.Fatal("owned provisioning not compensated")
	}
	var objects int
	if err := target.QueryRow(ctx, "SELECT (SELECT count(*) FROM pg_roles WHERE rolname LIKE 'data_%')+(SELECT count(*) FROM pg_database WHERE datname LIKE 'data_%')").Scan(&objects); err != nil || objects != 0 {
		t.Fatal("new database or role leaked")
	}
	cfg := target.Config().Copy()
	cfg.ConnConfig.Database = "source_data"
	source, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var sentinel string
	if err := source.QueryRow(ctx, "SELECT value FROM sentinel").Scan(&sentinel); err != nil || sentinel != "production" {
		t.Fatal("source data changed")
	}
}
