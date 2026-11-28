package link

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
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
	applied := map[int64]appliedRec{1: {id: 5, dests: []Destination{oai}}, 2: {id: 6, dests: []Destination{alt}}}
	p := planSidecar(defaultNames, planInput{nets: nets, saved: saved, applied: applied})
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
	p := planSidecar(defaultNames, planInput{nets: nets, preparing: map[int64][]Destination{3: nil}, prep: &prepareReq{app: 4, dests: []Destination{tg}}})
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
	p = planSidecar(defaultNames, planInput{saved: map[int64][]Destination{5: {oai}}, prep: &prepareReq{app: 5, dests: []Destination{tg}}})
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
	return &Egress{ctx: context.Background(), nets: f, names: defaultNames, preparing: map[int64]prepared{}, traffic: newTraffic()}
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

// The app service's own label (the selection its current spec was built
// with) and a prepare in flight keep their names too: a deploy not recorded
// yet — or interrupted by a restart of Krill — still has its hosts answered.
func TestPlanSidecarLabelAndPrepared(t *testing.T) {
	n := appNet(1, []string{"krill-1", SidecarService})
	n.Labels = map[string]map[string]string{"krill-1": {egressLabel: "api.openai.com:443,api.telegram.org:443"}, SidecarService: {egressLabel: "x.example.com:443"}}
	p := planSidecar(defaultNames, planInput{nets: []docker.PrefixNetwork{n, appNet(2, nil)}, preparing: map[int64][]Destination{2: {alt}}})
	if !reflect.DeepEqual(p.Keep["krill-egress-1"], []Destination{oai, tg}) {
		t.Fatalf("keep 1 = %v", p.Keep["krill-egress-1"])
	}
	if !reflect.DeepEqual(p.Keep["krill-egress-2"], []Destination{alt}) {
		t.Fatalf("keep 2 = %v", p.Keep["krill-egress-2"])
	}
}

// One app cannot take the session's whole stream budget: past its own bound,
// or the session's, new connections are refused locally.
func TestEgressStreamBounds(t *testing.T) {
	e := &Egress{streams: map[int64]int{}}
	for i := 0; i < maxAppStreams; i++ {
		if !e.takeStream(1) {
			t.Fatalf("stream %d refused", i)
		}
	}
	if e.takeStream(1) {
		t.Fatal("past the per-app bound")
	}
	if !e.takeStream(2) {
		t.Fatal("another app refused")
	}
	e.dropStream(1)
	if !e.takeStream(1) {
		t.Fatal("a freed stream not reusable")
	}
	e2 := &Egress{streams: map[int64]int{}}
	for i := 0; e2.takeStream(int64(i)); i++ {
	}
	if e2.total != MaxStreamsPerPeer-streamsReserved {
		t.Fatalf("session bound %d, want %d", e2.total, MaxStreamsPerPeer-streamsReserved)
	}
}

// A name the sidecar answers for leaves it only once it is needed by nothing
// (saved, applied, the service's label, a prepare), every live task of the
// app runs the current spec, and a deployment newer than the one current
// when the name began has recorded its selection.
func TestPlanSidecarServedNames(t *testing.T) {
	net := func(tasks ...uint64) docker.PrefixNetwork {
		n := appNet(1, []string{"krill-1", SidecarService})
		n.ForceUpdate = map[string]uint64{"krill-1": 3, SidecarService: 1}
		for i, fu := range tasks {
			n.Tasks = append(n.Tasks, docker.NetworkTask{Service: "krill-1", IP: "10.0.9." + strconv.Itoa(5+i), ForceUpdate: fu})
		}
		return n
	}
	served := map[int64][]servedRec{1: {{dest: tg, floor: 10}}}
	cases := []struct {
		name    string
		n       docker.PrefixNetwork
		saved   []Destination
		applied appliedRec
		keep    bool
	}{
		{"saved, then removed before any deploy", net(3), nil, appliedRec{id: 10}, true},
		{"no deployment recorded at all", net(3), nil, appliedRec{}, true},
		{"a newer deploy without it, still converging", net(3, 2), nil, appliedRec{id: 11}, true},
		{"a newer deploy without it, converged", net(3), nil, appliedRec{id: 11}, false},
		{"still saved", net(3), []Destination{tg}, appliedRec{id: 11}, true},
		{"still applied", net(3), nil, appliedRec{id: 11, dests: []Destination{tg}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planSidecar(defaultNames, planInput{nets: []docker.PrefixNetwork{c.n}, saved: map[int64][]Destination{1: c.saved},
				applied: map[int64]appliedRec{1: c.applied}, served: served})
			kept := slices.Contains(p.Keep["krill-egress-1"], tg)
			retired := slices.Contains(p.Retire[1], tg)
			if kept != c.keep || retired == c.keep {
				t.Fatalf("kept %v retired %v, want kept %v", kept, retired, c.keep)
			}
		})
	}
	// The service's label was rewritten (a domain change replaces the
	// labels): the served record still holds the name.
	n := net(3)
	n.Labels = map[string]map[string]string{"krill-1": {"traefik.enable": "true"}}
	p := planSidecar(defaultNames, planInput{nets: []docker.PrefixNetwork{n}, applied: map[int64]appliedRec{1: {id: 10}}, served: served})
	if !slices.Contains(p.Keep["krill-egress-1"], tg) {
		t.Fatal("a served name dropped when the label went")
	}
}
