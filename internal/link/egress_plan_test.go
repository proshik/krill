package link

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

var (
	tg  = Destination{"api.telegram.org", 443}
	oai = Destination{"api.openai.com", 443}
	alt = Destination{"api.openai.com", 8443}
)

func appNet(id int64, services []string, tasks ...docker.NetworkTask) docker.PrefixNetwork {
	return docker.PrefixNetwork{Name: EgressNetwork(id), Services: services, Tasks: tasks}
}

// Aliases are the saved selection ∪ the applied one: a host removed from the
// selection stays on the sidecar until a deploy without it has converged.
func TestPlanSidecarAliasesSavedUnionApplied(t *testing.T) {
	nets := []docker.PrefixNetwork{
		appNet(1, []string{"krill-1", SidecarService}),
		appNet(2, []string{"krill-2", SidecarService}),
	}
	saved := map[int64][]Destination{1: {tg}, 2: {}}
	applied := map[int64][]Destination{1: {oai}, 2: {alt}}
	p := planSidecar(nets, saved, applied, nil, nil)
	want := map[string][]Destination{
		"krill-egress-1": {oai, tg},
		"krill-egress-2": {alt},
	}
	if !reflect.DeepEqual(p.Keep, want) || len(p.Unused) != 0 {
		t.Fatalf("plan %+v, want keep %v", p, want)
	}
	in := p.input("http://203.0.113.10:8080"+EgressConfigPath, "t")
	if !reflect.DeepEqual(in.Aliases["krill-egress-1"], []string{"api.openai.com", "api.telegram.org"}) {
		t.Fatalf("aliases %v", in.Aliases)
	}
	spec, _ := sidecarSpec(in)
	if !reflect.DeepEqual(spec.NetworkAliases["krill-egress-2"], []string{"api.openai.com"}) {
		t.Fatalf("spec aliases %v", spec.NetworkAliases)
	}
}

// A network only the sidecar is on is left and then removed — unless its app
// is being prepared for a deploy; a prepare adds its destinations at once.
func TestPlanSidecarNetworks(t *testing.T) {
	nets := []docker.PrefixNetwork{
		appNet(1, []string{SidecarService}, docker.NetworkTask{Service: SidecarService, IP: "10.0.9.2"}),
		appNet(2, nil, docker.NetworkTask{Service: "krill-2", IP: "10.0.8.5"}), // a task still on it
		appNet(3, []string{SidecarService}),
		appNet(4, nil),
		{Name: "krill-egress-x"}, // not Krill's
	}
	p := planSidecar(nets, nil, nil, map[int64]bool{3: true}, &prepareReq{app: 4, dests: []Destination{tg}})
	if !reflect.DeepEqual(p.Unused, []string{"krill-egress-1"}) {
		t.Fatalf("unused %v", p.Unused)
	}
	if _, ok := p.Keep["krill-egress-2"]; !ok {
		t.Fatal("a network with a task of its app was dropped")
	}
	if _, ok := p.Keep["krill-egress-3"]; !ok {
		t.Fatal("a network being prepared was dropped")
	}
	if !reflect.DeepEqual(p.Keep["krill-egress-4"], []Destination{tg}) {
		t.Fatalf("prepared network keeps %v", p.Keep["krill-egress-4"])
	}
	// A network that did not show in the listing yet still gets the prepare.
	p = planSidecar(nil, map[int64][]Destination{5: {oai}}, nil, nil, &prepareReq{app: 5, dests: []Destination{tg}})
	if !reflect.DeepEqual(p.Keep["krill-egress-5"], []Destination{oai, tg}) {
		t.Fatalf("keep %v", p.Keep)
	}
}

type fakeNets struct {
	mu    sync.Mutex
	nets  []docker.PrefixNetwork
	calls int
}

func (f *fakeNets) PrefixNetworks(context.Context, string) ([]docker.PrefixNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.nets, nil
}

// egressWithNets is an Egress whose engine answers PrefixNetworks only.
func egressWithNets(f *fakeNets) *Egress {
	return &Egress{ctx: context.Background(), nets: f, preparing: map[int64]time.Time{}, traffic: newTraffic()}
}

// The app is the one whose egress network carries the sidecar address the
// connection was sent to, and the source must be that app's task on that
// network: another app's task, or an address the sidecar does not hold, is
// refused after one refresh.
func TestIdentify(t *testing.T) {
	indexRefreshMin = 0
	t.Cleanup(func() { indexRefreshMin = 250 * time.Millisecond })
	f := &fakeNets{nets: []docker.PrefixNetwork{
		appNet(7, nil,
			docker.NetworkTask{Service: SidecarService, IP: "10.0.7.2"},
			docker.NetworkTask{Service: "krill-7", IP: "10.0.7.5"}),
		appNet(8, nil,
			docker.NetworkTask{Service: SidecarService, IP: "10.0.8.2"},
			docker.NetworkTask{Service: "krill-8", IP: "10.0.8.5"},
			docker.NetworkTask{Service: "krill-9", IP: "10.0.8.9"}), // a foreign service on 8's network
	}}
	e := egressWithNets(f)
	addr := netip.MustParseAddr
	if id, err := e.identify(context.Background(), addr("10.0.7.5"), addr("10.0.7.2")); err != nil || id != 7 {
		t.Fatalf("app 7: %d, %v", id, err)
	}
	if f.calls != 1 {
		t.Fatalf("PrefixNetworks calls = %d, want 1 (the first miss)", f.calls)
	}
	if id, err := e.identify(context.Background(), addr("10.0.7.5"), addr("10.0.7.2")); err != nil || id != 7 || f.calls != 1 {
		t.Fatalf("cached: %d %v calls %d", id, err, f.calls)
	}
	for _, c := range []struct{ src, dst string }{
		{"10.0.8.5", "10.0.7.2"}, // app 8's task to app 7's sidecar address
		{"10.0.8.9", "10.0.8.2"}, // a foreign service on app 8's network
		{"10.0.7.5", "10.0.7.9"}, // not the sidecar
	} {
		before := f.calls
		if _, err := e.identify(context.Background(), addr(c.src), addr(c.dst)); !errors.Is(err, ErrUnknownSource) {
			t.Fatalf("%s → %s: %v, want ErrUnknownSource", c.src, c.dst, err)
		}
		if f.calls != before+1 {
			t.Fatalf("%s → %s: %d refreshes, want one", c.src, c.dst, f.calls-before)
		}
	}
	// A task that just started is found by the refresh.
	f.mu.Lock()
	f.nets[0].Tasks = append(f.nets[0].Tasks, docker.NetworkTask{Service: "krill-7", IP: "10.0.7.6"})
	f.mu.Unlock()
	if id, err := e.identify(context.Background(), addr("10.0.7.6"), addr("10.0.7.2")); err != nil || id != 7 {
		t.Fatalf("new task: %d, %v", id, err)
	}
}
