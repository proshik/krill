package link

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/docker"
)

// egressNames are the names egress gives Swarm objects. Production uses
// defaultNames; an integration test on a shared Swarm uses its own.
type egressNames struct {
	sidecar    string             // the sidecar service
	netPrefix  string             // the per-app egress networks
	appService func(int64) string // an app's service
	objects    string             // the label value of the sidecar's secrets
}

var defaultNames = egressNames{sidecar: SidecarService, netPrefix: EgressNetworkPrefix,
	appService: docker.ServiceName, objects: sidecarObjectValue}

func (n egressNames) network(appID int64) string {
	return n.netPrefix + strconv.FormatInt(appID, 10)
}

// appOf is the app of an egress network, false for any other name.
func (n egressNames) appOf(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, n.netPrefix)
	if !ok {
		return 0, false
	}
	return EgressNetworkApp(EgressNetworkPrefix + rest)
}

// sidecarPlan is what the sidecar should serve: per egress network it stays
// on, the destinations whose hosts it answers for there, and the networks
// nothing uses any more, which it leaves (and which are then removed).
type sidecarPlan struct {
	Keep   map[string][]Destination
	Unused []string
}

// prepareReq is a deploy about to put an app on its egress network.
type prepareReq struct {
	app   int64
	dests []Destination
}

// planSidecar decides the sidecar's networks and names. A network stays while
// a service other than the sidecar attaches it or a task of one is on it, or
// while its app is being prepared for a deploy. On a network it stays on, the
// sidecar answers for the app's saved selection and the one its running
// deployment applied: a host leaves DNS only once a deploy without it has
// converged, so until then the app's connections to it are refused, never
// sent to the real address.
func planSidecar(names egressNames, nets []docker.PrefixNetwork, saved, applied map[int64][]Destination, preparing map[int64]bool, prep *prepareReq) sidecarPlan {
	p := sidecarPlan{Keep: map[string][]Destination{}}
	for _, n := range nets {
		appID, ok := names.appOf(n.Name)
		if !ok {
			continue // not one of Krill's: leave it alone
		}
		isPrep := prep != nil && prep.app == appID
		if !usedByApps(n, names.sidecar) && !preparing[appID] && !isPrep {
			p.Unused = append(p.Unused, n.Name)
			continue
		}
		p.Keep[n.Name] = unionDests(saved[appID], applied[appID])
	}
	if prep != nil {
		name := names.network(prep.app)
		p.Keep[name] = unionDests(p.Keep[name], saved[prep.app], applied[prep.app], prep.dests)
	}
	slices.Sort(p.Unused)
	return p
}

// usedByApps reports whether anything but the sidecar uses n.
func usedByApps(n docker.PrefixNetwork, sidecar string) bool {
	for _, s := range n.Services {
		if s != sidecar {
			return true
		}
	}
	for _, t := range n.Tasks {
		if t.Service != sidecar {
			return true
		}
	}
	return false
}

func unionDests(sets ...[]Destination) []Destination {
	var all []Destination
	for _, s := range sets {
		all = append(all, s...)
	}
	out := SortDestinations(all)
	if out == nil {
		out = []Destination{}
	}
	return out
}

// input turns a plan into the sidecar's input: hosts per network, and one
// entrypoint per distinct port.
func (p sidecarPlan) input(providerURL, token string) sidecarInput {
	in := sidecarInput{Aliases: map[string][]string{}, ProviderURL: providerURL, Token: token}
	seen := map[int]bool{}
	for n, ds := range p.Keep {
		hosts := []string{}
		for _, d := range ds {
			hosts = append(hosts, d.Host)
			if !seen[d.Port] {
				seen[d.Port] = true
				in.Ports = append(in.Ports, d.Port)
			}
		}
		in.Aliases[n] = hosts
	}
	return in
}

// egressIndex maps the addresses on the egress networks to apps: the
// sidecar's address on an app's network names the app (the PROXY header's
// destination), and the app's own tasks there are the only sources it
// accepts.
type egressIndex struct {
	sidecar map[netip.Addr]int64
	tasks   map[int64]map[netip.Addr]bool
}

func buildIndex(names egressNames, nets []docker.PrefixNetwork) egressIndex {
	x := egressIndex{sidecar: map[netip.Addr]int64{}, tasks: map[int64]map[netip.Addr]bool{}}
	for _, n := range nets {
		appID, ok := names.appOf(n.Name)
		if !ok {
			continue
		}
		own := names.appService(appID)
		for _, t := range n.Tasks {
			ip, err := netip.ParseAddr(t.IP)
			if err != nil {
				continue
			}
			ip = ip.Unmap()
			switch t.Service {
			case names.sidecar:
				x.sidecar[ip] = appID
			case own:
				if x.tasks[appID] == nil {
					x.tasks[appID] = map[netip.Addr]bool{}
				}
				x.tasks[appID][ip] = true
			}
		}
	}
	return x
}

// lookup identifies the app of a connection from its PROXY addresses.
func (x egressIndex) lookup(src, dst netip.Addr) (int64, bool) {
	appID, ok := x.sidecar[dst.Unmap()]
	if !ok || !x.tasks[appID][src.Unmap()] {
		return 0, false
	}
	return appID, true
}
