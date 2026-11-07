package envclone

import (
	"context"
	"io"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/testutil"
)

type migratingEngine struct {
	docker.Engine
	execs int
}

func (e *migratingEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (e *migratingEngine) Exec(_ context.Context, _ string, _ []string, _ []string, _ io.Reader, out io.Writer) error {
	e.execs++
	_, err := io.WriteString(out, "t\n")
	return err
}
func TestMigratingInstanceBlocksCloneCleanupAndPrune(t *testing.T) {
	for _, stage := range []string{"clone", "cleanup", "prune"} {
		t.Run(stage, func(t *testing.T) {
			ctx := t.Context()
			pool := testutil.NewTestDB(t)
			q := db.New(pool)
			r := cloneFixture(t, pool, "migrating")
			inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: r.OrgID, Engine: "postgres", Name: "pg", AppName: "owned-migrating", Image: "postgres:16-alpine", Superuser: "fixture", SuperuserPassword: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: inst.ID, EnvironmentID: r.SourceID, Name: "source", DbName: "source", Username: "source", Password: "fixture"}); err != nil {
				t.Fatal(err)
			}
			if err = q.UpdateDBInstanceStatus(ctx, db.UpdateDBInstanceStatusParams{ID: inst.ID, Status: "migrating"}); err != nil {
				t.Fatal(err)
			}
			engine := &migratingEngine{}
			s := New(pool, dbservice.New(engine, dbservice.NewDBStore(q), nil, "fixture"), engine, "fixture.test")
			defer s.Shutdown(context.Background())
			if stage == "clone" {
				id, err := s.Clone(ctx, r)
				if err == nil || id != 0 {
					t.Error("migrating source published")
				}
			} else {
				reserved, err := s.journal.Reserve(ctx, db.InsertEnvironmentCloneParams{RequestKey: r.Key, UserID: r.UserID, OrganizationID: r.OrgID, ProjectID: r.ProjectID, SourceID: r.SourceID, DestinationName: r.Name, DestinationSlug: r.Name, Deadline: time.Now().Add(-time.Minute)})
				if err != nil {
					t.Fatal(err)
				}
				// The intent carries the deadline as stored, like Clone does:
				// Postgres keeps microseconds, and Linux clocks have nanoseconds.
				if err = s.journal.RecordIntent(ctx, r.Key, ResourceIntent{Databases: []DatabaseIntent{{InstanceID: inst.ID, InstanceAppName: inst.AppName, Database: dbservice.CloneDatabase{Operation: r.Key, DBName: "owned_new", Username: "owned_new", Deadline: reserved.Deadline}}}}); err != nil {
					t.Fatal(err)
				}
				if stage == "prune" {
					if err = s.journal.StartCleanup(ctx, r.Key); err != nil {
						t.Fatal(err)
					}
					if err = s.journal.CompleteCleanup(ctx, r.Key); err != nil {
						t.Fatal(err)
					}
				}
				s.Recover(ctx)
				op, err := s.journal.Outcome(ctx, r.Key)
				if err != nil {
					t.Fatal(err)
				}
				if stage == "cleanup" && op.State != "cleanup" {
					t.Error("unfinished cleanup not retained")
				}
				if stage == "prune" && op.FencesPruned {
					t.Error("unperformed prune marked complete")
				}
			}
			if engine.execs != 0 {
				t.Errorf("migrating instance received %d SQL execs", engine.execs)
			}
			envs, err := q.ListEnvironments(ctx, r.ProjectID)
			if err != nil || len(envs) != 1 {
				t.Error("target published")
			}
		})
	}
}
