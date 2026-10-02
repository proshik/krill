package hostshare

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"sync"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

type fixture struct {
	q      *db.Queries
	store  *Store
	envID  int64
	env2ID int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "o@k.local", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	svc := org.NewService(q)
	o, _ := svc.CreateOrg(ctx, u.ID, "Org")
	p, _ := svc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := svc.CreateEnvironment(ctx, p.ID, "production")
	e2, _ := svc.CreateEnvironment(ctx, p.ID, "staging")
	return fixture{q: q, store: NewStore(pool), envID: e.ID, env2ID: e2.ID}
}

func (f fixture) app(t *testing.T, envID int64, name string) int64 {
	t.Helper()
	a, err := f.q.CreateApplication(context.Background(), db.CreateApplicationParams{
		EnvironmentID: envID, Name: name, Image: "nginx", Tag: "alpine",
		Domain: name + ".primary.test", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestStoreAddSharesAndInheritsTLS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web, api := f.app(t, f.envID, "web"), f.app(t, f.envID, "api")

	if _, err := f.store.Add(ctx, AddParams{AppID: web, Host: "m.test", TLS: true}); err != nil {
		t.Fatal(err)
	}
	d, err := f.store.Add(ctx, AddParams{AppID: api, Host: "m.test", TLS: false, Exposed: true, Paths: []string{"/api/"}})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Tls {
		t.Error("a row on an existing host must inherit its TLS")
	}
	if d.Paths != "/api" {
		t.Errorf("paths stored as %q, want normalized /api", d.Paths)
	}
}

// Review Focus 2: an internal (exposed=false) catch-all still claims the host.
func TestStoreInternalCatchAllStillClaims(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web, other := f.app(t, f.envID, "web"), f.app(t, f.envID, "other")
	if _, err := f.store.Add(ctx, AddParams{AppID: web, Host: "m.test", Exposed: false}); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.Add(ctx, AddParams{AppID: other, Host: "m.test", Exposed: true})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Key != KeyCatchAllTaken {
		t.Fatalf("want catch-all conflict, got %v", err)
	}
}

func TestStoreRejectsAnotherEnvironment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web, stg := f.app(t, f.envID, "web"), f.app(t, f.env2ID, "stg")
	if _, err := f.store.Add(ctx, AddParams{AppID: web, Host: "m.test"}); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.Add(ctx, AddParams{AppID: stg, Host: "m.test", Paths: []string{"/x"}})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Key != KeyHostTaken {
		t.Fatalf("want host taken, got %v", err)
	}
}

// Review Focus 3: saving your own row unchanged is not a conflict.
func TestStoreUpdatePathsOwnRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web, api := f.app(t, f.envID, "web"), f.app(t, f.envID, "api")
	if _, err := f.store.Add(ctx, AddParams{AppID: web, Host: "m.test"}); err != nil {
		t.Fatal(err)
	}
	d, err := f.store.Add(ctx, AddParams{AppID: api, Host: "m.test", Paths: []string{"/api"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdatePaths(ctx, d.ID, true, []string{"/api"}); err != nil {
		t.Fatalf("unchanged save: %v", err)
	}
	if err := f.store.UpdatePaths(ctx, d.ID, true, nil); err == nil {
		t.Fatal("turning the api row into a second catch-all must fail")
	}
	got, _ := f.q.GetDomain(ctx, d.ID)
	if got.Paths != "/api" || !got.Exposed {
		t.Fatalf("row = %+v", got)
	}
}

func TestStoreSetTLSSwitchesTheWholeHost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web, api := f.app(t, f.envID, "web"), f.app(t, f.envID, "api")
	d1, _ := f.store.Add(ctx, AddParams{AppID: web, Host: "m.test"})
	d2, _ := f.store.Add(ctx, AddParams{AppID: api, Host: "m.test", Paths: []string{"/api"}})
	refs, err := f.store.SetTLS(ctx, d1.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("want 2 affected apps, got %+v", refs)
	}
	for _, id := range []int64{d1.ID, d2.ID} {
		if d, _ := f.q.GetDomain(ctx, id); !d.Tls {
			t.Errorf("domain %d: TLS not switched", id)
		}
	}
}

// Two concurrent claims of the catch-all: the lock lets exactly one through.
func TestStoreConcurrentCatchAll(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.app(t, f.envID, "a"), f.app(t, f.envID, "b")
	for i := 0; i < 10; i++ {
		host := "race" + string(rune('a'+i)) + ".test"
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for j, app := range []int64{a, b} {
			wg.Add(1)
			go func(j int, app int64) {
				defer wg.Done()
				<-start
				_, errs[j] = f.store.Add(ctx, AddParams{AppID: app, Host: host})
			}(j, app)
		}
		close(start)
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("%s: want exactly one success, got %v / %v", host, errs[0], errs[1])
		}
	}
}

// Primary app creation and panel reservation must share Add's lock: removing
// the unique index must not allow a concurrent request to claim a foreign host.
func TestStoreConcurrentExclusiveReservations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.q.InitPanelGateway(ctx, "test-secret"); err != nil {
		t.Fatal(err)
	}
	foreign := f.app(t, f.env2ID, "foreign")
	for _, kind := range []string{"primary", "panel"} {
		t.Run(kind, func(t *testing.T) {
			for i := 0; i < 10; i++ {
				host := fmt.Sprintf("%s%d.example.test", kind, i)
				start := make(chan struct{})
				errs := make(chan error, 2)
				go func() {
					<-start
					_, err := f.store.Add(ctx, AddParams{AppID: foreign, Host: host, Paths: []string{"/api"}})
					errs <- err
				}()
				go func() {
					<-start
					if kind == "panel" {
						errs <- f.store.SetPanelDomain(ctx, host)
						return
					}
					_, err := f.store.CreateApplication(ctx, db.CreateApplicationParams{
						EnvironmentID: f.envID, Name: fmt.Sprintf("new%d", i), Domain: host,
						Image: "nginx", Tag: "alpine", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
					})
					errs <- err
				}()
				close(start)
				a, b := <-errs, <-errs
				if (a == nil) == (b == nil) {
					t.Fatalf("want one successful reservation, got %v / %v", a, b)
				}
				failed := a
				if failed == nil {
					failed = b
				}
				if key := conflictKey(t, failed); key != KeyHostTaken {
					t.Fatalf("unexpected conflict: %v", failed)
				}
			}
		})
	}
}

func TestStoreCreateApplicationRollsBackOnDomainFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A failed domain insert must leave neither a partial app nor a reservation.
	_, err := f.store.pool.Exec(ctx, `CREATE FUNCTION refuse_domain() RETURNS trigger LANGUAGE plpgsql AS $$
    BEGIN RAISE EXCEPTION 'injected domain failure'; END $$;
    CREATE TRIGGER refuse_domain BEFORE INSERT ON domains FOR EACH ROW EXECUTE FUNCTION refuse_domain()`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: f.envID, Name: "broken", Domain: "broken.example.test",
		Image: "nginx", Tag: "alpine", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err == nil {
		t.Fatal("expected domain failure")
	}
	apps, err := f.q.ListApplicationsByEnvironment(ctx, f.envID)
	if err != nil || len(apps) != 0 {
		t.Fatalf("orphan applications: %v, %v", apps, err)
	}
	if _, err := f.store.pool.Exec(ctx, "DROP TRIGGER refuse_domain ON domains"); err != nil {
		t.Fatal(err)
	}
	app := f.app(t, f.envID, "retry")
	if _, err := f.store.Add(ctx, AddParams{AppID: app, Host: "broken.example.test"}); err != nil {
		t.Fatal(err)
	}
}

// An in-flight TLS request for a deleted domain must not change a new tenant
// that claimed the same host while the request waited on its advisory lock.
func TestStoreTLSDoesNotMutateAReclaimedHost(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := f.app(t, f.envID, "old"), f.app(t, f.env2ID, "new")
	old, err := f.store.Add(ctx, AddParams{AppID: a, Host: "reclaimed.test"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "krill.domain-host:reclaimed.test"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.store.SetTLS(ctx, old.ID, true); done <- err }()
	for {
		var waiting int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
            AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("TLS request did not reach the host lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := f.q.DeleteDomain(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: b, Host: old.Host})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected missing original domain, got %v", err)
	}
	got, err := f.q.GetDomain(ctx, replacement.ID)
	if err != nil || got.Tls {
		t.Fatalf("replacement changed: %+v, %v", got, err)
	}
}
