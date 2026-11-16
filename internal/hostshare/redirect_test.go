package hostshare

import (
	"context"
	"errors"
	"testing"

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
