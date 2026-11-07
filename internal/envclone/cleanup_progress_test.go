package envclone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"reflect"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/testutil"
)

// Each process lifetime can finish one RPC. The next transport call waits for
// deterministic shutdown cancellation rather than spending a real 30s budget.
type prefixCleanupEngine struct {
	docker.Engine
	cancel       context.CancelFunc
	calls        int
	successes    *[]string
	afterSuccess func()
}

func (e *prefixCleanupEngine) Exec(ctx context.Context, name string, _ []string, _ []string, _ io.Reader, _ io.Writer) error {
	e.calls++
	if e.calls == 1 {
		*e.successes = append(*e.successes, name)
		if e.afterSuccess != nil {
			e.afterSuccess()
		}
		return nil
	}
	e.cancel()
	<-ctx.Done()
	return ctx.Err()
}

type failedProgressDB struct {
	journalDB
	fail bool
}
type failedProgressRow struct{}

func (failedProgressRow) Scan(...any) error { return errors.New("progress metadata unavailable") }
func (d *failedProgressDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if d.fail {
		return failedProgressRow{}
	}
	return d.journalDB.QueryRow(ctx, sql, args...)
}

func TestCleanupProgressSurvivesProcessRecreation(t *testing.T) {
	for _, mode := range []string{"cancel-next-rpc", "cancel-after-success", "progress-write-error", "invalid-cursor"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			pool := testutil.NewTestDB(t)
			q := db.New(pool)
			r := cloneFixture(t, pool, "cleanup-prefix")
			j := NewJournal(pool)
			deadline := time.Now().Add(time.Minute)
			reserved, err := j.Reserve(ctx, db.InsertEnvironmentCloneParams{RequestKey: r.Key, UserID: r.UserID, OrganizationID: r.OrgID, ProjectID: r.ProjectID, SourceID: r.SourceID, DestinationName: r.Name, DestinationSlug: r.Name, Deadline: deadline})
			if err != nil {
				t.Fatal(err)
			}
			// Use the persisted timestamp precision and omit Go's monotonic clock.
			// UTC, because the intent is compared with reflect.DeepEqual after a
			// JSON round trip, which yields UTC for a "Z" offset: a Local time
			// would differ in its location under TZ=UTC (as on CI) though equal.
			deadline = reserved.Deadline.UTC()
			intent := ResourceIntent{}
			names := []string{}
			for i := range 3 {
				name := fmt.Sprintf("owned-prefix-%d", i)
				names = append(names, name)
				inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: r.OrgID, Engine: "postgres", Name: name, AppName: name, Image: "postgres:16-alpine", Superuser: "fixture", SuperuserPassword: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				intent.Databases = append(intent.Databases, DatabaseIntent{InstanceID: inst.ID, InstanceAppName: name, Database: dbservice.CloneDatabase{Operation: r.Key, DBName: "owned_new", Username: "owned_new", Deadline: deadline}})
			}
			if err := j.RecordIntent(ctx, r.Key, intent); err != nil {
				t.Fatal(err)
			}
			if mode == "invalid-cursor" {
				for _, invalid := range []int{-1, len(intent.Databases) + 1} {
					if _, err := pool.Exec(ctx, "UPDATE environment_clones SET resource_intent=jsonb_set(resource_intent::jsonb,'{cleaned_databases}',to_jsonb($2::integer))::text WHERE request_key=$1", r.Key, invalid); err != nil {
						t.Fatal(err)
					}
					var successes []string
					engine := &prefixCleanupEngine{successes: &successes}
					s := New(pool, dbservice.New(engine, dbservice.NewDBStore(q), nil, "fixture"), engine, "fixture.test")
					engine.cancel = s.cancel
					s.Recover(ctx)
					if err := s.Shutdown(ctx); err != nil {
						t.Fatal(err)
					}
					op, err := j.Outcome(ctx, r.Key)
					if err != nil || op.State != "cleanup" || engine.calls != 0 {
						t.Fatal("invalid cleanup cursor did not fail closed")
					}
				}
				return
			}
			var successes []string
			passes := 3
			want := names
			if mode == "cancel-after-success" {
				passes = 4
			} // Final state transition may need a fresh context.
			if mode == "progress-write-error" {
				passes = 4
				want = append([]string{names[0]}, names...)
			}
			for pass := range passes {
				engine := &prefixCleanupEngine{successes: &successes}
				s := New(pool, dbservice.New(engine, dbservice.NewDBStore(q), nil, "fixture"), engine, "fixture.test")
				engine.cancel = s.cancel
				if mode == "cancel-after-success" {
					engine.afterSuccess = s.cancel
				}
				if mode == "progress-write-error" && pass == 0 {
					boundary := &failedProgressDB{journalDB: pool}
					s.journal = NewJournal(boundary)
					engine.afterSuccess = func() { boundary.fail = true }
				}
				s.Recover(ctx)
				if mode == "progress-write-error" && pass == 0 {
					op, err := j.Outcome(ctx, r.Key)
					if err != nil {
						t.Fatal(err)
					}
					var got ResourceIntent
					if json.Unmarshal([]byte(op.ResourceIntent), &got) != nil || got.CleanedDatabases != 0 || op.State != "cleanup" {
						t.Fatal("failed metadata write skipped unconfirmed cleanup")
					}
				}
				if err := s.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
			op, err := j.Outcome(ctx, r.Key)
			if err != nil {
				t.Fatal(err)
			}
			if op.State != "aborted" || !reflect.DeepEqual(successes, want) {
				t.Fatalf("cleanup did not advance across restarts: state=%s successes=%v", op.State, successes)
			}
			var completed ResourceIntent
			if json.Unmarshal([]byte(op.ResourceIntent), &completed) != nil || completed.CleanedDatabases != len(intent.Databases) || !reflect.DeepEqual(completed.Databases, intent.Databases) {
				t.Fatal("cleanup changed identities or lost confirmed progress")
			}
			// A fresh request can reclaim the slug only after all external cleanup passed.
			r.Key, err = NewRequestKey()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := j.Reserve(ctx, db.InsertEnvironmentCloneParams{RequestKey: r.Key, UserID: r.UserID, OrganizationID: r.OrgID, ProjectID: r.ProjectID, SourceID: r.SourceID, DestinationName: r.Name, DestinationSlug: r.Name, Deadline: deadline}); err != nil {
				t.Fatal("cleanup retained slug", err)
			}
		})
	}
}
