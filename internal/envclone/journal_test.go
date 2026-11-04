package envclone

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

func TestJournalAtomicPublicationAndRecovery(t *testing.T) {
	ctx := t.Context()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	j := NewJournal(pool)
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "clone@fixture.test", PasswordHash: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	os := org.NewService(q)
	o, err := os.CreateOrg(ctx, u.ID, "clone fixture")
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.CreateProject(ctx, o.ID, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := os.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	request := func(n int, slug string) db.InsertEnvironmentCloneParams {
		return db.InsertEnvironmentCloneParams{RequestKey: fmt.Sprintf("%032x", n), UserID: u.ID, OrganizationID: o.ID, ProjectID: p.ID, SourceID: e.ID, DestinationName: slug, DestinationSlug: slug, Deadline: time.Now().Add(time.Minute)}
	}
	reserve := func(r db.InsertEnvironmentCloneParams) db.EnvironmentClone {
		t.Helper()
		op, err := j.Reserve(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}

	t.Run("request identity and active reservation", func(t *testing.T) {
		r := request(1, "staging")
		op := reserve(r)
		r.Deadline = r.Deadline.Add(time.Hour)
		if got := reserve(r); !got.Deadline.Equal(op.Deadline) {
			t.Fatal("replay extended provisioning authorization")
		}
		other := r
		other.SourceID++
		if _, err := j.Reserve(ctx, other); !errors.Is(err, ErrRequestConflict) {
			t.Fatalf("request rebound: %v", err)
		}
		if _, err := j.Reserve(ctx, request(2, "staging")); !errors.Is(err, ErrNameConflict) {
			t.Fatalf("duplicate name accepted: %v", err)
		}
		if err := j.RecordIntent(ctx, r.RequestKey, ResourceIntent{}); err != nil {
			t.Fatal(err)
		}
		if err := j.RecordIntent(ctx, r.RequestKey, ResourceIntent{}); !errors.Is(err, ErrState) {
			t.Fatal("intent was mutable")
		}
		if err := j.StartCleanup(ctx, r.RequestKey); err != nil {
			t.Fatal(err)
		}
		called := false
		if _, err := j.Publish(ctx, r.RequestKey, func(pgx.Tx) (int64, error) { called = true; return 1, nil }); !errors.Is(err, ErrState) || called {
			t.Fatal("cleanup raced publication")
		}
		if err := j.CompleteCleanup(ctx, r.RequestKey); err != nil {
			t.Fatal(err)
		}
		reserve(request(2, "staging")) // failed reservation inserted no historical row
	})

	t.Run("outer rollback includes app primary host and journal", func(t *testing.T) {
		r := request(3, "rollback")
		reserve(r)
		if err := j.RecordIntent(ctx, r.RequestKey, ResourceIntent{}); err != nil {
			t.Fatal(err)
		}
		injected := errors.New("failure after app insertion")
		_, err := j.Publish(ctx, r.RequestKey, func(tx pgx.Tx) (int64, error) {
			tq := db.New(tx)
			env, err := tq.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: r.DestinationName, Slug: r.DestinationSlug})
			if err != nil {
				return 0, err
			}
			_, err = hostshare.CreateApplicationTx(ctx, tx, db.CreateApplicationParams{EnvironmentID: env.ID, Name: "web", Domain: "rollback.clone.test", SourceType: "image", Image: "fixture", Port: 8080})
			if err != nil {
				return 0, err
			}
			return 0, injected
		})
		if !errors.Is(err, injected) {
			t.Fatal(err)
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM environments WHERE project_id=$1 AND slug='rollback'", p.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("partial environment published")
		}
		if count, err := q.CountDomainsByHost(ctx, "rollback.clone.test"); err != nil || count != 0 {
			t.Fatal("primary host escaped rollback")
		}
		if op, err := j.Outcome(ctx, r.RequestKey); err != nil || op.State != "preparing" || op.TargetID != nil {
			t.Fatal("failed transaction marked success")
		}
	})

	t.Run("acknowledged success replays and slug can be reused after delete", func(t *testing.T) {
		r := request(4, "published")
		reserve(r)
		if err := j.RecordIntent(ctx, r.RequestKey, ResourceIntent{}); err != nil {
			t.Fatal(err)
		}
		target, err := j.Publish(ctx, r.RequestKey, func(tx pgx.Tx) (int64, error) {
			row, err := db.New(tx).CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: r.DestinationName, Slug: r.DestinationSlug})
			return row.ID, err
		})
		if err != nil {
			t.Fatal(err)
		}
		// This checks acknowledged publication replay. The coordinator integration
		// test injects a lost COMMIT reply at the real pgx transaction boundary.
		op, err := j.Outcome(context.WithoutCancel(ctx), r.RequestKey)
		if err != nil || op.TargetID == nil || *op.TargetID != target || op.State != "succeeded" {
			t.Fatal("publication outcome missing")
		}
		if err := j.StartCleanup(ctx, r.RequestKey); !errors.Is(err, ErrState) {
			t.Fatal("published resources could be compensated")
		}
		if got := reserve(r); got.TargetID == nil || *got.TargetID != target {
			t.Fatal("successful replay lost identity")
		}
		if _, err := j.Reserve(ctx, request(5, "published")); !errors.Is(err, ErrNameConflict) {
			t.Fatal("unrelated same-name environment accepted")
		}
		if err := q.DeleteEnvironment(ctx, target); err != nil {
			t.Fatal(err)
		}
		reserve(request(5, "published"))
		if got := reserve(r); got.TargetID == nil || *got.TargetID != target {
			t.Fatal("old request was rebound after target deletion")
		}
	})

	t.Run("execution lease excludes another coordinator", func(t *testing.T) {
		key := request(6, "lease").RequestKey
		release, err := j.Acquire(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := j.Acquire(ctx, key); !errors.Is(err, ErrBusy) {
			t.Fatalf("duplicate executor acquired lease: %v", err)
		}
		release()
		release()
		again, err := j.Acquire(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		again()
	})

	t.Run("pending cleanup survives project deletion", func(t *testing.T) {
		r := request(7, "cleanup")
		reserve(r)
		if err := j.StartCleanup(ctx, r.RequestKey); err != nil {
			t.Fatal(err)
		}
		if err := q.DeleteProject(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		op, err := j.Outcome(ctx, r.RequestKey)
		if err != nil || op.State != "cleanup" || op.SourceID != e.ID {
			t.Fatal("project deletion lost ownership journal")
		}
		if err := j.CompleteCleanup(ctx, r.RequestKey); err != nil {
			t.Fatal(err)
		}
	})
}
