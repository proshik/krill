package deploy

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
)

// An app with an applied link selection gets its own egress network as a
// second network; Traefik keeps routing it over the organization network
// through the traefik.docker.network label, and no traefik.swarm.* label is
// added beside it (Traefik v3.6.1 skips a service carrying both kinds).
func TestBuildSpecEgressNetwork(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	a := imageApp()
	a.Network = "krill-org-3"
	a.LinkEgress = []string{"api.telegram.org:443"}
	sp := d.buildSpec(a, "img")
	if !reflect.DeepEqual(sp.Networks, []string{"krill-org-3", "krill-egress-1"}) {
		t.Fatalf("networks = %v", sp.Networks)
	}
	if sp.Network != "krill-org-3" || sp.Labels["traefik.docker.network"] != "krill-org-3" {
		t.Fatalf("network %q, traefik network label %q", sp.Network, sp.Labels["traefik.docker.network"])
	}
	for k := range sp.Labels {
		if strings.HasPrefix(k, "traefik.swarm.") {
			t.Fatalf("label %s beside traefik.docker.network", k)
		}
	}
	if sp.KeepNetworkPrefix != "" {
		t.Fatalf("KeepNetworkPrefix = %q: this deployer manages the egress attachment itself", sp.KeepNetworkPrefix)
	}

	// No applied selection: the organization network only, so a deploy
	// after the link was turned off or revoked leaves the egress network.
	a.LinkEgress = nil
	sp = d.buildSpec(a, "img")
	if len(sp.Networks) != 0 || sp.Network != "krill-org-3" || sp.KeepNetworkPrefix != "" {
		t.Fatalf("without egress: networks %v network %q keep %q", sp.Networks, sp.Network, sp.KeepNetworkPrefix)
	}
}

type fakeEgress struct {
	mu       sync.Mutex
	prepared [][]string
	changed  []int64
	err      error
	deployed func() int // ServiceDeploy calls seen when Prepare ran
}

func (f *fakeEgress) Prepare(_ context.Context, appID int64, dests []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deployed != nil && f.deployed() != 0 {
		return errors.New("prepared after the service was deployed")
	}
	f.prepared = append(f.prepared, dests)
	return f.err
}

func (f *fakeEgress) Changed(appID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changed = append(f.changed, appID)
}

// Before an app deploys onto its egress network the link prepares it (the
// network and the sidecar's aliases); after the deploy the link is told, so
// the sidecar drops a host a deploy without it no longer needs.
func TestDeployPreparesEgress(t *testing.T) {
	secret.Init("")
	eng := &mockEngine{}
	a := imageApp()
	a.LinkEgress = []string{"api.telegram.org:443"}
	st := newFakeStore(a)
	d := newDeployer(eng, &mockBuilder{}, st)
	eg := &fakeEgress{deployed: func() int { eng.mu.Lock(); defer eng.mu.Unlock(); return len(eng.deployed) }}
	d.SetEgress(eg)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	waitFor(t, func() bool { eg.mu.Lock(); defer eg.mu.Unlock(); return len(eg.changed) == 1 })
	eg.mu.Lock()
	defer eg.mu.Unlock()
	if !reflect.DeepEqual(eg.prepared, [][]string{{"api.telegram.org:443"}}) || eg.changed[0] != 1 {
		t.Fatalf("prepared %v changed %v", eg.prepared, eg.changed)
	}
}

// A failed preparation fails the deploy before the service is touched: the
// app would otherwise start on a network whose names resolve to the real
// addresses.
func TestDeployFailsWhenEgressPrepareFails(t *testing.T) {
	secret.Init("")
	eng := &mockEngine{}
	a := imageApp()
	a.LinkEgress = []string{"api.telegram.org:443"}
	st := newFakeStore(a)
	d := newDeployer(eng, &mockBuilder{}, st)
	d.SetEgress(&fakeEgress{err: errors.New("sidecar did not start")})
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.deployed) != 0 {
		t.Fatalf("service deployed despite the failed preparation: %+v", eng.deployed)
	}
}

// No hook wired and a selection to apply: the deploy fails rather than
// attach a network nothing serves.
func TestDeployWithEgressNeedsTheHook(t *testing.T) {
	secret.Init("")
	eng := &mockEngine{}
	a := imageApp()
	a.LinkEgress = []string{"api.telegram.org:443"}
	st := newFakeStore(a)
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()
	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.deployed) != 0 {
		t.Fatalf("deployed without the link: %+v", eng.deployed)
	}
}

// What a deploy applied keeps the selection in plain form: the link reads it
// to keep a host's alias until a deploy without it has converged.
func TestAppliedConfigCarriesEgress(t *testing.T) {
	secret.Init("")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	a := imageApp()
	a.LinkEgress = []string{"api.openai.com:443", "api.telegram.org:443"}
	c := d.configFingerprint(a)
	if !reflect.DeepEqual(c.Egress, a.LinkEgress) || c.Sections[SectionLink] == "" {
		t.Fatalf("applied %+v", c)
	}
	a.LinkEgress = nil
	if c := d.configFingerprint(a); c.Egress != nil {
		t.Fatalf("egress without a selection = %v", c.Egress)
	}
	_ = docker.ServiceName
}

// A fingerprint recorded before the link section existed applied no
// selection: the first selection saved after the update shows as not applied,
// and the app's next spec carries it on the service label.
func TestPendingLinkAfterUpgrade(t *testing.T) {
	q, st, d, _, appID, _ := configFixture(t)
	ctx := t.Context()
	first := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, first)
	requireDeployment(t, q, first, "done")
	// As the link core release recorded it: no link section.
	if _, err := st.pool.Exec(ctx, `UPDATE deployments SET applied_config = applied_config #- '{sections,link}' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if got := d.PendingChanges(ctx, []int64{appID})[appID]; got.Any() {
		t.Fatalf("pending without a selection = %+v", got)
	}
	ident, err := q.GetApplicationIdentity(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO link_settings (id, role) VALUES (true, 'client') ON CONFLICT (id) DO UPDATE SET role = 'client'`,
		`INSERT INTO link_org_grants (org_id) VALUES ($1)`,
	} {
		args := []any{}
		if strings.Contains(stmt, "$1") {
			args = append(args, ident.OrgID)
		}
		if _, err := st.pool.Exec(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO app_link_destinations (application_id, host, port) VALUES ($1, 'api.telegram.org', 443)`, appID); err != nil {
		t.Fatal(err)
	}
	if got := d.PendingChanges(ctx, []int64{appID})[appID]; !reflect.DeepEqual(got.Changed, []string{SectionLink}) {
		t.Fatalf("pending after the first selection = %+v, want [link]", got)
	}
	a, err := st.GetApplication(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if sp := d.buildSpec(a, "img"); sp.Labels[LinkEgressLabel] != "api.telegram.org:443" {
		t.Fatalf("selection label = %q", sp.Labels[LinkEgressLabel])
	}
}

// After a rollback the core release's deploy records no link section; after
// the update again, emptying the selection shows as not applied, because the
// selection in force is the one the last egress deploy applied.
func TestPendingLinkAfterRollback(t *testing.T) {
	q, st, d, _, appID, _ := configFixture(t)
	ctx := t.Context()
	ident, err := q.GetApplicationIdentity(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO link_settings (id, role) VALUES (true, 'client') ON CONFLICT (id) DO UPDATE SET role = 'client'`,
		`INSERT INTO link_org_grants (org_id) VALUES (` + strconv.FormatInt(ident.OrgID, 10) + `)`,
		`INSERT INTO app_link_destinations (application_id, host, port) VALUES (` + strconv.FormatInt(appID, 10) + `, 'api.telegram.org', 443)`,
	} {
		if _, err := st.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	d.SetEgress(&fakeEgress{})
	egressDeploy := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, egressDeploy)
	requireDeployment(t, q, egressDeploy, "done")
	// The core release's deploy after a rollback: no link section.
	rollbackDeploy := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, rollbackDeploy)
	requireDeployment(t, q, rollbackDeploy, "done")
	if _, err := st.pool.Exec(ctx, `UPDATE deployments SET applied_config = (applied_config #- '{sections,link}') - 'egress' WHERE id = $1`, rollbackDeploy); err != nil {
		t.Fatal(err)
	}
	if got := d.PendingChanges(ctx, []int64{appID})[appID]; got.Has(SectionLink) {
		t.Fatalf("pending with the selection unchanged = %+v", got)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM app_link_destinations WHERE application_id = $1`, appID); err != nil {
		t.Fatal(err)
	}
	if got := d.PendingChanges(ctx, []int64{appID})[appID]; !got.Has(SectionLink) {
		t.Fatalf("emptying the selection after the rollback: pending = %+v, want link", got)
	}
}
