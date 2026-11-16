package hostshare

import (
	"context"
	"errors"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
)

// appWithPrimary creates an app the way the server does: app + primary row.
func (f fixture) appWithPrimary(t *testing.T, envID int64, name string) db.Application {
	t.Helper()
	a, err := f.store.CreateApplication(context.Background(), db.CreateApplicationParams{
		EnvironmentID: envID, Name: name, Image: "nginx", Tag: "alpine",
		Domain: name + ".primary.test", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func primaryOf(t *testing.T, f fixture, appID int64) db.Domain {
	t.Helper()
	doms, err := f.q.ListDomainsByApplication(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	var out []db.Domain
	for _, d := range doms {
		if d.IsPrimary {
			out = append(out, d)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want exactly one primary, got %d", len(out))
	}
	return out[0]
}

func wantKey(t *testing.T, err error, key string) {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Key != key {
		t.Fatalf("want conflict %s, got %v", key, err)
	}
}

func TestStoreSetRedirect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web := f.appWithPrimary(t, f.envID, "web")
	old, err := f.store.Add(ctx, AddParams{AppID: web.ID, Host: "old.test", Paths: []string{"/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetRedirect(ctx, primaryOf(t, f, web.ID).ID, true); err == nil {
		t.Fatal("redirect on the primary must be refused")
	} else {
		wantKey(t, err, KeyRedirectOnPrimary)
	}
	if err := f.store.SetRedirect(ctx, old.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.q.GetDomain(ctx, old.ID); !got.RedirectToPrimary {
		t.Fatal("redirect not stored")
	}
	// The whole host is taken now, even by paths that would not overlap.
	api := f.appWithPrimary(t, f.envID, "api")
	_, err = f.store.Add(ctx, AddParams{AppID: api.ID, Host: "old.test", Paths: []string{"/api"}})
	wantKey(t, err, KeyHostRedirects)
	if err := f.store.SetRedirect(ctx, old.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.q.GetDomain(ctx, old.ID); got.RedirectToPrimary {
		t.Fatal("redirect not cleared")
	}
	// The schema backs the primary rule up.
	if _, err := f.q.SetDomainRedirect(ctx, db.SetDomainRedirectParams{ID: primaryOf(t, f, web.ID).ID, Redirect: true}); err != nil {
		t.Fatal(err)
	}
	if primaryOf(t, f, web.ID).RedirectToPrimary {
		t.Fatal("conditional update changed the primary")
	}
}

func TestStoreSetRedirectRefusesASharedHost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web, api := f.appWithPrimary(t, f.envID, "web"), f.appWithPrimary(t, f.envID, "api")
	mine, err := f.store.Add(ctx, AddParams{AppID: web.ID, Host: "m.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Add(ctx, AddParams{AppID: api.ID, Host: "m.test", Paths: []string{"/api"}}); err != nil {
		t.Fatal(err)
	}
	wantKey(t, f.store.SetRedirect(ctx, mine.ID, true), KeyHostRedirects)
}

func TestStoreMakePrimary(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web := f.appWithPrimary(t, f.envID, "web")
	before := primaryOf(t, f, web.ID)
	next, err := f.store.Add(ctx, AddParams{AppID: web.ID, Host: "new.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetRedirect(ctx, next.ID, true); err != nil {
		t.Fatal(err)
	}
	wantKey(t, f.store.MakePrimary(ctx, next.ID), KeyPrimaryRedirects)
	if err := f.store.SetRedirect(ctx, next.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MakePrimary(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	if got := primaryOf(t, f, web.ID); got.ID != next.ID {
		t.Fatalf("primary = %s", got.Host)
	}
	app, _ := f.q.GetApplication(ctx, web.ID)
	if app.Domain != "new.test" {
		t.Fatalf("applications.domain = %s", app.Domain)
	}
	// Idempotent, and the former primary can now redirect to the new one.
	if err := f.store.MakePrimary(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetRedirect(ctx, before.ID, true); err != nil {
		t.Fatalf("redirect the former primary: %v", err)
	}
}

func TestStoreMakePrimaryRefusesASharedHost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web, api := f.appWithPrimary(t, f.envID, "web"), f.appWithPrimary(t, f.envID, "api")
	mine, err := f.store.Add(ctx, AddParams{AppID: web.ID, Host: "m.test", Paths: []string{"/web"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Add(ctx, AddParams{AppID: api.ID, Host: "m.test", Paths: []string{"/api"}}); err != nil {
		t.Fatal(err)
	}
	wantKey(t, f.store.MakePrimary(ctx, mine.ID), KeyPrimaryShared)
	if primaryOf(t, f, web.ID).Host != "web.primary.test" {
		t.Fatal("primary moved despite the refusal")
	}
}

// Two primary changes of one app to different hosts must not deadlock: each
// locks its own target row, then rewrites every row of the app. A third
// transaction holding row D (stored before B and C) parks both changes after
// they locked their targets; releasing it would let them deadlock without the
// application lock.
func TestStoreConcurrentMakePrimary(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web := f.appWithPrimary(t, f.envID, "web")
	var ids []int64
	for _, host := range []string{"d.test", "b.test", "c.test"} {
		d, err := f.store.Add(ctx, AddParams{AppID: web.ID, Host: host})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	blocker, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, "SELECT id FROM domains WHERE id = $1 FOR UPDATE", ids[0]); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, id := range ids[1:] {
		go func(id int64) { errs <- f.store.MakePrimary(ctx, id) }(id)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := f.store.pool.QueryRow(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("both primary changes never waited (%d waiting)", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 2; k++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent make primary: %v", err)
		}
	}
	p := primaryOf(t, f, web.ID)
	app, _ := f.q.GetApplication(ctx, web.ID)
	if app.Domain != p.Host {
		t.Fatalf("applications.domain %s, primary row %s", app.Domain, p.Host)
	}
}

// A delete that read the row before it became primary removes nothing.
func TestDeleteNonPrimaryDomainAfterMakePrimary(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web := f.appWithPrimary(t, f.envID, "web")
	b, err := f.store.Add(ctx, AddParams{AppID: web.ID, Host: "b.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MakePrimary(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.q.DeleteNonPrimaryDomain(ctx, b.ID); err != nil || n != 0 {
		t.Fatalf("deleted %d rows (err %v): the primary must survive", n, err)
	}
	if primaryOf(t, f, web.ID).ID != b.ID {
		t.Fatal("primary lost")
	}
}
