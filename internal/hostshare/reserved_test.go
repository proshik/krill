package hostshare

import (
	"context"
	"fmt"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// relayOn makes the installation a relay routing linkHost.
func (f fixture) relayOn(t *testing.T, linkHost string) {
	t.Helper()
	ctx := context.Background()
	if err := f.q.InitLinkSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetLinkRelay(ctx, db.SetLinkRelayParams{LinkHost: &linkHost}); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) ingressHost(t *testing.T, host string) {
	t.Helper()
	ctx := context.Background()
	peer, err := f.q.CreateLinkPeer(ctx, db.CreateLinkPeerParams{Name: "msk", CertSha256: "aa"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, "INSERT INTO link_ingress_hosts (host, peer_id) VALUES ($1, $2)", host, peer.ID); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) panelOn(t *testing.T, host string) {
	t.Helper()
	ctx := context.Background()
	if err := f.q.InitPanelGateway(ctx, "test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetPanelDomainPending(ctx, host); err != nil {
		t.Fatal(err)
	}
}

func wantConflict(t *testing.T, err error, key string) {
	t.Helper()
	if got := conflictKey(t, err); got != key {
		t.Fatalf("conflict %q, want %q (err %v)", got, key, err)
	}
}

// Every domain write refuses a host the installation itself holds: the panel
// domain, the relay's link host and a relay ingress host.
func TestReservedHostRefusedByEveryCaller(t *testing.T) {
	cases := []struct {
		name    string
		reserve func(f fixture, t *testing.T, host string)
		key     string
	}{
		{"panel", func(f fixture, t *testing.T, h string) { f.panelOn(t, h) }, KeyReservedPanel},
		{"link host", func(f fixture, t *testing.T, h string) { f.relayOn(t, h) }, KeyReservedLink},
		{"ingress host", func(f fixture, t *testing.T, h string) { f.ingressHost(t, h) }, KeyReservedLink},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			web := f.app(t, f.envID, "web")
			const host = "held.example.com"
			// A domain that sat on the host before it was reserved (a rollback
			// to a release that did not know the reservation).
			legacy, err := f.store.Add(ctx, AddParams{AppID: web, Host: host, Paths: []string{"/old"}})
			if err != nil {
				t.Fatal(err)
			}
			tc.reserve(f, t, host)

			t.Run("Add", func(t *testing.T) {
				_, err := f.store.Add(ctx, AddParams{AppID: web, Host: host, Paths: []string{"/api"}})
				wantConflict(t, err, tc.key)
			})
			t.Run("CreateApplication", func(t *testing.T) {
				_, err := f.store.CreateApplication(ctx, db.CreateApplicationParams{
					EnvironmentID: f.envID, Name: "new", Domain: host,
					Image: "nginx", Tag: "alpine", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
				})
				wantConflict(t, err, tc.key)
			})
			t.Run("ReservePrimaryHost", func(t *testing.T) {
				tx, err := f.store.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				wantConflict(t, ReservePrimaryHost(ctx, tx, host), tc.key)
			})
			t.Run("MakePrimary", func(t *testing.T) {
				wantConflict(t, f.store.MakePrimary(ctx, legacy.ID), tc.key)
			})
			t.Run("Reserved", func(t *testing.T) {
				r, err := Reserved(ctx, f.q, "HELD.example.com ")
				if err != nil {
					t.Fatal(err)
				}
				if r.Key() != tc.key {
					t.Fatalf("Reserved = %q (key %q), want key %q", r, r.Key(), tc.key)
				}
			})
		})
	}
}

// A host is free again once the relay role is off, and other hosts never were
// reserved.
func TestReservedHostOnlyWhileRelay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web := f.app(t, f.envID, "web")
	f.relayOn(t, "link.example.com")
	if _, err := f.store.Add(ctx, AddParams{AppID: web, Host: "other.example.com"}); err != nil {
		t.Fatalf("an unrelated host must stay free: %v", err)
	}
	if err := f.q.SetLinkRoleOff(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := Reserved(ctx, f.q, "link.example.com"); err != nil || r != NotReserved {
		t.Fatalf("Reserved after role off = %q, %v", r, err)
	}
}

func TestSetPanelDomainRefusesLinkHosts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.q.InitPanelGateway(ctx, "test-secret"); err != nil {
		t.Fatal(err)
	}
	f.relayOn(t, "link.example.com")
	f.ingressHost(t, "hook.example.com")
	wantConflict(t, f.store.SetPanelDomain(ctx, "link.example.com"), KeyReservedLink)
	wantConflict(t, f.store.SetPanelDomain(ctx, "hook.example.com"), KeyReservedLink)
	if err := f.store.SetPanelDomain(ctx, "panel.example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestReserveLinkHost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web := f.app(t, f.envID, "web")
	if _, err := f.store.Add(ctx, AddParams{AppID: web, Host: "app.example.com"}); err != nil {
		t.Fatal(err)
	}
	f.panelOn(t, "panel.example.com")
	wrote := 0
	write := func(q *db.Queries) error { wrote++; return nil }

	wantConflict(t, f.store.ReserveLinkHost(ctx, "app.example.com", write), KeyHostTaken)
	wantConflict(t, f.store.ReserveLinkHost(ctx, "panel.example.com", write), KeyReservedPanel)
	if wrote != 0 {
		t.Fatalf("write ran %d times on a refused host", wrote)
	}
	if err := f.store.ReserveLinkHost(ctx, "link.example.com", write); err != nil || wrote != 1 {
		t.Fatalf("free host: err %v, wrote %d", err, wrote)
	}
}

// The link host and an application domain race for one host: exactly one wins.
func TestReserveLinkHostRacesAdd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.q.InitLinkSettings(ctx); err != nil {
		t.Fatal(err)
	}
	web := f.app(t, f.envID, "web")
	for i := 0; i < 10; i++ {
		host := fmt.Sprintf("race%d.example.com", i)
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			_, err := f.store.Add(ctx, AddParams{AppID: web, Host: host})
			errs <- err
		}()
		go func() {
			<-start
			errs <- f.store.ReserveLinkHost(ctx, host, func(q *db.Queries) error {
				h := host
				return q.SetLinkRelay(ctx, db.SetLinkRelayParams{LinkHost: &h})
			})
		}()
		close(start)
		a, b := <-errs, <-errs
		if (a == nil) == (b == nil) {
			t.Fatalf("host %s: want exactly one winner, got %v / %v", host, a, b)
		}
	}
}

func TestLinkHostHolders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	web := f.app(t, f.envID, "web")
	got, err := f.store.LinkHostHolders(ctx, "link.example.com")
	if err != nil || len(got) != 0 {
		t.Fatalf("free host: %v, %v", got, err)
	}
	if _, err := f.store.Add(ctx, AddParams{AppID: web, Host: "link.example.com"}); err != nil {
		t.Fatal(err)
	}
	f.panelOn(t, "link.example.com")
	got, err = f.store.LinkHostHolders(ctx, "link.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Panel || got[1].AppID != web || got[1].AppName != "web" || got[1].Env != "production" || got[1].Project != "Proj" {
		t.Fatalf("holders = %+v", got)
	}
}
