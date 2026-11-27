package deploy

import (
	"context"
	"errors"
	"reflect"
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
