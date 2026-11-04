package envclone

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

type unavailableCloneEngine struct {
	docker.Engine
	execs int
}

func (e *unavailableCloneEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true}, nil
}
func (e *unavailableCloneEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	e.execs++
	return errors.New("server unreachable")
}

func TestStoppedSourceAndUnreachableRecoveryRemainUnpublished(t *testing.T) {
	ctx := t.Context()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "recovery@fixture.test", PasswordHash: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	os := org.NewService(q)
	o, err := os.CreateOrg(ctx, u.ID, "recovery")
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.CreateProject(ctx, o.ID, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "postgres", Name: "pg", AppName: "owned-recovery-pg", Image: "postgres:16-alpine", Superuser: "fixture", SuperuserPassword: "fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: inst.ID, EnvironmentID: env.ID, Name: "data", DbName: "source", Username: "source", Password: "source-password"})
	if err != nil {
		t.Fatal(err)
	}
	engine := &unavailableCloneEngine{}
	s := New(pool, dbservice.New(engine, dbservice.NewDBStore(q), nil, "fixture"), engine, "fixture.test")
	defer s.Shutdown(context.Background())
	key, _ := NewRequestKey()
	if _, err := s.Clone(ctx, Request{Key: key, Name: "staging", UserID: u.ID, OrgID: o.ID, ProjectID: p.ID, SourceID: env.ID}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stopped PG accepted: %v", err)
	}
	if engine.execs != 0 {
		t.Fatal("stopped source received provisioning")
	}
	op, err := s.journal.Outcome(ctx, key)
	if err != nil || op.State != "aborted" {
		t.Fatal("stopped source not cleanly aborted")
	}
	key, _ = NewRequestKey()
	op, err = s.journal.Reserve(ctx, db.InsertEnvironmentCloneParams{RequestKey: key, UserID: u.ID, OrganizationID: o.ID, ProjectID: p.ID, SourceID: env.ID, DestinationName: "staging", DestinationSlug: "staging", Deadline: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.journal.RecordIntent(ctx, key, ResourceIntent{Databases: []DatabaseIntent{{InstanceID: inst.ID, InstanceAppName: inst.AppName, Database: dbservice.CloneDatabase{Operation: key, DBName: "owned_new", Username: "owned_new", Deadline: op.Deadline}}}}); err != nil {
		t.Fatal(err)
	}
	s.Recover(ctx)
	op, err = s.journal.Outcome(ctx, key)
	if err != nil || op.State != "cleanup" || op.ResourceIntent == "" {
		t.Fatal("outage lost ownership tracking")
	}
	if engine.execs != 1 || s.InFlight() != 0 {
		t.Fatal("recovery did not finish its bounded attempt")
	}
	envs, err := q.ListEnvironments(ctx, p.ID)
	if err != nil || len(envs) != 1 {
		t.Fatal("partial target was published")
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clone(ctx, Request{}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("clone accepted during shutdown")
	}
	pool.Close()
	if err := s.checkCaller(ctx, Request{UserID: u.ID, OrgID: o.ID, ProjectID: p.ID, SourceID: env.ID}); !errors.Is(err, ErrFailed) {
		t.Fatalf("metadata outage misclassified: %v", err)
	}

}

type cancellingCleanupEngine struct {
	docker.Engine
	cancel context.CancelFunc
}

func (e *cancellingCleanupEngine) Exec(ctx context.Context, _ string, _ []string, _ []string, _ io.Reader, _ io.Writer) error {
	e.cancel()
	<-ctx.Done()
	return ctx.Err()
}

func TestRecoveryCancellationDoesNotStarveNextOperation(t *testing.T) {
	ctx := t.Context()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	r := cloneFixture(t, pool, "fair-recovery")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: r.OrgID, Engine: "postgres", Name: "pg", AppName: "owned-fair-pg", Image: "postgres:16-alpine", Superuser: "fixture", SuperuserPassword: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	engine := &cancellingCleanupEngine{}
	s := New(pool, dbservice.New(engine, dbservice.NewDBStore(q), nil, "fixture"), engine, "fixture.test")
	defer s.Shutdown(context.Background())
	reserve := func(key, name string) {
		_, err := s.journal.Reserve(ctx, db.InsertEnvironmentCloneParams{RequestKey: key, UserID: r.UserID, OrganizationID: r.OrgID, ProjectID: r.ProjectID, SourceID: r.SourceID, DestinationName: name, DestinationSlug: name, Deadline: time.Now().Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
	}
	reserve(r.Key, "old")
	op, err := s.journal.Outcome(ctx, r.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.journal.RecordIntent(ctx, r.Key, ResourceIntent{Databases: []DatabaseIntent{{InstanceID: inst.ID, InstanceAppName: inst.AppName, Database: dbservice.CloneDatabase{Operation: r.Key, DBName: "owned", Username: "owned", Deadline: op.Deadline}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.journal.Publish(ctx, r.Key, func(tx pgx.Tx) (int64, error) {
		e, err := db.New(tx).CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: r.ProjectID, Name: "old", Slug: "old"})
		return e.ID, err
	}); err != nil {
		t.Fatal(err)
	}
	next, _ := NewRequestKey()
	reserve(next, "healthy")
	if _, err := pool.Exec(ctx, "UPDATE environment_clones SET updated_at=now()-interval '2 minutes' WHERE request_key=$1", r.Key); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		pass, cancel := context.WithCancel(ctx)
		engine.cancel = cancel
		s.Recover(pass)
		cancel()
	}
	got, err := s.journal.Outcome(ctx, next)
	if err != nil || got.State != "aborted" {
		t.Fatalf("healthy cleanup starved after cancelled pruning: state=%s err=%v", got.State, err)
	}
}
