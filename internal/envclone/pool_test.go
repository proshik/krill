package envclone

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

func cloneFixture(t *testing.T, pool *pgxpool.Pool, name string) Request {
	t.Helper()
	ctx := t.Context()
	q := db.New(pool)
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: name + "@fixture.test", PasswordHash: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	os := org.NewService(q)
	o, err := os.CreateOrg(ctx, u.ID, name)
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
	key, err := NewRequestKey()
	if err != nil {
		t.Fatal(err)
	}
	return Request{Key: key, Name: "staging", UserID: u.ID, OrgID: o.ID, ProjectID: p.ID, SourceID: e.ID}
}

// dbDeadline is a clone deadline offset from the database's clock. Recovery
// tells a passed deadline by the database's now() (deadline <= now()), so a
// deadline taken from the host's time.Now() turns from pending to passed, or
// back, when the database container's clock drifts from the host's by more
// than the offset.
func dbDeadline(t *testing.T, pool *pgxpool.Pool, offset time.Duration) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(t.Context(), "SELECT now() + make_interval(secs => $1)", offset.Seconds()).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestCloneProgressWithSinglePoolConnection(t *testing.T) {
	base := testutil.NewTestDB(t)
	cfg := base.Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	r := cloneFixture(t, pool, "single-connection")
	svc := New(pool, nil, nil, "fixture.test")
	defer svc.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	id, err := svc.Clone(ctx, r)
	if err != nil || id == 0 {
		t.Fatalf("one-connection clone made no progress: %v", err)
	}
	// Recovery uses the same guard and must leave the only pool slot available.
	svc.Recover(ctx)
	if _, err := pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("panel pool unavailable: %v", err)
	}
}
