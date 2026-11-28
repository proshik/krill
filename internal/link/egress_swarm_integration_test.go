//go:build integration

package link

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/testutil"
)

// swarmHostAddr is how a Swarm task reaches this test's listeners: Colima's
// address of the host (KRILL_IT_HOST_ADDR overrides it).
func swarmHostAddr() string {
	if a := os.Getenv("KRILL_IT_HOST_ADDR"); a != "" {
		return a
	}
	return "192.168.5.2"
}

// krl002 names keep this test's Swarm objects apart from anything else on a
// shared Swarm.
var krl002Names = egressNames{sidecar: "krl002-sidecar", netPrefix: "krl002-egress-",
	appService: func(id int64) string { return "krl002-app-" + strconv.FormatInt(id, 10) }, objects: "krl002-egress-config"}

const swarmDest = "api.example.test"

// egressFixture is a client paired with an in-process relay whose single
// destination is an HTTPS server on this host, with the client's egress on
// the real Swarm.
type egressFixture struct {
	relay, client install
	pool          *pgxpool.Pool
	q             *db.Queries
	eng           docker.Engine
	e             *Egress
	orgNet        string
}

func newEgressFixture(t *testing.T) *egressFixture {
	t.Helper()
	ctx := context.Background()
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Nodes(ctx); err != nil {
		t.Skipf("no Swarm: %v", err)
	}
	f := &egressFixture{eng: eng}

	// The destination: an HTTPS server the relay dials in place of the real
	// host.
	kp, err := NewSelfSigned(swarmDest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := tlsCert(kp)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "egress ok") })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	f.relay = newInstall(t, freePort(t))
	if err := f.relay.svc.SetRelay(ctx, testLinkHost); err != nil {
		t.Fatal(err)
	}
	relayDialsTo(f.relay, ln.Addr().String())
	gw := startGateway(t, f.relay)

	// The client: its UI listener serves the sidecar's configuration, its
	// link listener takes the sidecar's connections; both on every
	// interface, so the Swarm task reaches them.
	f.pool = testutil.NewTestDB(t)
	f.q = db.New(f.pool)
	uiLn, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	uiPort := uiLn.Addr().(*net.TCPAddr).Port
	linkPort := freePort(t)
	svc := NewService(f.pool, hostshare.NewStore(f.pool), Config{
		AdvertiseAddr: swarmHostAddr(), ListenAddr: ":" + strconv.Itoa(uiPort),
		LinkListenAddr: ":" + strconv.Itoa(linkPort), Version: "v0.0.0-test",
	})
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	f.client = install{svc: svc, port: linkPort}
	if err := svc.SetClient(ctx); err != nil {
		t.Fatal(err)
	}
	code, err := f.relay.svc.CreatePairingCode(ctx, "msk", gw.addr())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Pair(ctx, code); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the session", 20*time.Second, func() bool { return svc.ClientStatus().State == StateConnected })
	if err := f.relay.svc.AddDestination(ctx, Destination{swarmDest, 443}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the relay's list", 10*time.Second, func() bool {
		ds, _ := svc.ClientRelayDestinations()
		return len(ds) == 1
	})

	sec, err := panel.EnsureSecret(ctx, f.q)
	if err != nil {
		t.Fatal(err)
	}
	f.e, err = NewEgress(svc, eng, sec)
	if err != nil {
		t.Fatal(err)
	}
	f.e.names = krl002Names
	if err := f.e.Requirement(); err != nil {
		t.Fatal(err)
	}
	ui := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EgressConfigPath || !panel.Matches(r.Header.Get(EgressHeader), f.e.Token()) {
			http.NotFound(w, r)
			return
		}
		cfg, err := f.e.DynamicConfig(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(cfg)
	})}
	go func() { _ = ui.Serve(uiLn) }()
	t.Cleanup(func() { _ = ui.Close() })

	f.orgNet = "krl002-org-" + strconv.FormatInt(time.Now().UnixNano()%1e6, 10)
	if _, err := eng.NetworkEnsure(ctx, f.orgNet); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

// cleanup removes every krl002 object this test made.
func (f *egressFixture) cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	nets, _ := f.eng.(docker.PrefixNetworks).PrefixNetworks(ctx, "krl002-")
	for _, n := range nets {
		for _, s := range n.Services {
			if strings.HasPrefix(s, "krl002-") {
				_ = f.eng.ServiceRemove(ctx, s)
			}
		}
	}
	_ = f.eng.ServiceRemove(ctx, krl002Names.sidecar)
	_ = f.eng.(docker.SwarmObjects).PruneObjects(ctx, sidecarObjectLabel, krl002Names.objects, nil)
	for ctx.Err() == nil {
		nets, err := f.eng.(docker.PrefixNetworks).PrefixNetworks(ctx, "krl002-")
		if err != nil || len(nets) == 0 {
			return
		}
		for _, n := range nets {
			_ = f.eng.NetworkRemove(ctx, n.Name)
		}
		time.Sleep(time.Second)
	}
	t.Error("krl002 networks left behind")
}

// app creates an app in an organization (granted or not) with a saved
// selection, and returns its id.
func (f *egressFixture) app(t *testing.T, name string, granted bool, dests ...Destination) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projID, envID, appID int64
	var userID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, name+"@example.com").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO organizations (name, slug, owner_id) VALUES ($1, $1, $2) RETURNING id`, "krl002-"+name, userID).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO projects (organization_id, name, slug) VALUES ($1, 'bots', 'bots') RETURNING id`, orgID).Scan(&projID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO environments (project_id, name, slug) VALUES ($1, 'prod', 'prod') RETURNING id`, projID).Scan(&envID); err != nil {
		t.Fatal(err)
	}
	a, err := f.q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: envID, Name: name, Image: "alpine", Tag: "3",
		Domain: name + ".example.test", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	appID = a.ID
	if granted {
		if err := f.client.svc.SetGrant(ctx, orgID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.client.svc.SetAppDestinations(ctx, orgID, appID, dests); err != nil {
			t.Fatal(err)
		}
	}
	return appID
}

// applied records a successful deployment of the app that applied dests.
func (f *egressFixture) applied(t *testing.T, appID int64, dests ...string) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"v": 2, "key": "", "sections": map[string]string{}, "egress": dests})
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO deployments (application_id, status, trigger, image_tag, applied_config, started_at)
		VALUES ($1, 'done', 'manual', 'alpine:3', $2, now())`, appID, cfg); err != nil {
		t.Fatal(err)
	}
}

// runApp deploys the app's service on the organization network and, when
// egress is set, its egress network; it waits until a task runs.
func (f *egressFixture) runApp(t *testing.T, appID int64, egress bool) string {
	t.Helper()
	ctx := context.Background()
	name := krl002Names.appService(appID)
	nets := []string{f.orgNet}
	if egress {
		nets = append(nets, krl002Names.network(appID))
	}
	spec := docker.ServiceSpec{Name: name, Image: "alpine:3", Replicas: 1, Network: f.orgNet, Networks: nets,
		Command: []string{"sh", "-c", `trap 'exit 0' TERM; sleep 3600 & wait`}}
	if err := f.eng.ServiceDeploy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := waitService(ctx, f.eng, name, nil); err != nil {
		t.Fatal(err)
	}
	return name
}

// in runs sh -c cmd in the service's container.
func (f *egressFixture) in(t *testing.T, service, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := f.eng.Exec(ctx, service, []string{"sh", "-c", cmd}, nil, nil, &out)
	return out.String(), err
}

// fetch is an HTTPS request from inside the app to the destination by name.
const fetch = `wget -qO- -T 8 --no-check-certificate https://` + swarmDest + `/ 2>&1; echo "exit=$?"`

// resolve prints the addresses the app resolves the destination to.
const resolve = `getent hosts ` + swarmDest + ` | cut -d' ' -f1; true`

func (f *egressFixture) sidecarIP(t *testing.T, appID int64) string {
	t.Helper()
	nets, err := f.eng.(docker.PrefixNetworks).PrefixNetworks(context.Background(), krl002Names.netPrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		if n.Name != krl002Names.network(appID) {
			continue
		}
		for _, tk := range n.Tasks {
			if tk.Service == krl002Names.sidecar && tk.State == "running" {
				return tk.IP
			}
		}
	}
	return ""
}

// TestEgressOnSwarm runs the egress path on a real Swarm with the pinned
// Traefik as the sidecar and the Krill listener on this host: an app on its
// egress network reaches the destination by its usual name through the link,
// and Krill saw the app's own task address in the PROXY header (it identified
// the app by it); an app outside that network does not resolve the alias; a
// destination removed from the selection is refused, not resolved to the real
// address, until a deploy without it has converged; the sidecar at zero
// replicas refuses.
func TestEgressOnSwarm(t *testing.T) {
	if testing.Short() {
		t.Skip("swarm")
	}
	sidecarConverge = 90 * time.Second
	t.Cleanup(func() { sidecarConverge = 3 * time.Minute })
	f := newEgressFixture(t)
	ctx := context.Background()
	d := Destination{swarmDest, 443}
	appID := f.app(t, "notifier", true, d)
	other := f.app(t, "other", false)

	if err := f.e.Prepare(ctx, appID, []string{d.String()}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	svc := f.runApp(t, appID, true)
	f.e.Changed(appID)
	otherSvc := f.runApp(t, other, false)

	t.Run("the app reaches the destination through the link", func(t *testing.T) {
		var out string
		eventually(t, "egress through the sidecar", 60*time.Second, func() bool {
			out, _ = f.in(t, svc, fetch)
			return strings.Contains(out, "egress ok")
		})
		got, _ := f.in(t, svc, resolve)
		if ip := f.sidecarIP(t, appID); ip == "" || strings.TrimSpace(got) != ip {
			t.Fatalf("the app resolves %s to %q, want the sidecar's task address %q (DNSRR)", swarmDest, got, ip)
		}
		var cell Traffic
		for _, c := range f.e.traffic.of(0) {
			if c.Service == appID {
				cell = c
			}
		}
		if cell.Conns == 0 || cell.Down == 0 {
			t.Fatalf("no traffic counted for the app: %+v (%s)", cell, out)
		}
	})

	t.Run("an app outside the egress network does not resolve the alias", func(t *testing.T) {
		got, _ := f.in(t, otherSvc, resolve)
		if ip := f.sidecarIP(t, appID); strings.Contains(got, ip) {
			t.Fatalf("the other app resolves %s to the sidecar (%q)", swarmDest, got)
		}
	})

	t.Run("a removed destination is refused until a deploy without it converges", func(t *testing.T) {
		f.applied(t, appID, d.String())
		orgID := orgOf(t, f, appID)
		if err := f.client.svc.SetAppDestinations(ctx, orgID, appID, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.e.reconcile(ctx, nil); err != nil {
			t.Fatal(err)
		}
		ip := f.sidecarIP(t, appID)
		if got, _ := f.in(t, svc, resolve); strings.TrimSpace(got) != ip {
			t.Fatalf("while still applied the app resolves %q, want the sidecar %q", got, ip)
		}
		if out, _ := f.in(t, svc, fetch); strings.Contains(out, "egress ok") {
			t.Fatal("a destination no longer selected went through")
		}
		// The deploy without it converged: the alias goes.
		f.applied(t, appID)
		if err := f.e.reconcile(ctx, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the alias to go", 30*time.Second, func() bool {
			got, _ := f.in(t, svc, resolve)
			return !strings.Contains(got, ip) && !strings.Contains(got, f.sidecarIP(t, appID)+"\n")
		})
	})

	t.Run("the sidecar at zero replicas refuses", func(t *testing.T) {
		orgID := orgOf(t, f, appID)
		if err := f.client.svc.SetAppDestinations(ctx, orgID, appID, []Destination{d}); err != nil {
			t.Fatal(err)
		}
		if err := f.e.reconcile(ctx, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "egress again", 60*time.Second, func() bool {
			out, _ := f.in(t, svc, fetch)
			return strings.Contains(out, "egress ok")
		})
		ip := f.sidecarIP(t, appID)
		if err := f.eng.ServiceScale(ctx, krl002Names.sidecar, 0); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the sidecar to stop", 60*time.Second, func() bool {
			st, err := f.eng.ServiceState(ctx, krl002Names.sidecar)
			return err == nil && st.Running == 0
		})
		// The stopping task keeps its name for a moment after Swarm reports
		// it stopped (a request may still go through the link then — never
		// directly); then the name does not resolve at all (DNSRR at zero),
		// never to the real address.
		eventually(t, "the name to stop resolving", 30*time.Second, func() bool {
			got := strings.TrimSpace(func() string { s, _ := f.in(t, svc, resolve); return s }())
			if got != "" && got != ip {
				t.Fatalf("with the sidecar stopping the app resolves %q", got)
			}
			return got == ""
		})
		if out, _ := f.in(t, svc, fetch); strings.Contains(out, "egress ok") {
			t.Fatal("egress with the sidecar at zero")
		}
	})

	t.Run("the sidecar and the network go once no app uses them", func(t *testing.T) {
		for _, s := range []string{svc, otherSvc} {
			if err := f.eng.ServiceRemove(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, "the egress network and the sidecar to go", 90*time.Second, func() bool {
			time.Sleep(time.Second)
			if err := f.e.reconcile(ctx, nil); err != nil {
				return false
			}
			st, err := f.eng.ServiceState(ctx, krl002Names.sidecar)
			nets, nerr := f.eng.(docker.PrefixNetworks).PrefixNetworks(ctx, krl002Names.netPrefix)
			return err == nil && !st.Found && nerr == nil && len(nets) == 0
		})
	})
}

func orgOf(t *testing.T, f *egressFixture, appID int64) int64 {
	t.Helper()
	_, org, ok, err := f.client.svc.store.appPath(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("app %d: %v", appID, err)
	}
	return org
}
