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

// egressLabel is the service label with the selection an app's current
// spec was built with (deploy.LinkEgressLabel).
const egressLabel = "krill.link.egress"

// labelSelection reads egressLabel.
func labelSelection(labels map[string]string) []Destination {
	var out []Destination
	for _, s := range strings.Split(labels[egressLabel], ",") {
		if d, err := ParseDestinationString(s); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// sidecarPlan is what the sidecar should serve: per egress network it stays
// on, the destinations whose hosts it answers for there, and the networks
// nothing uses any more, which it leaves (and which are then removed). Retire
// are the served names (by app) a converged deploy without them retired.
type sidecarPlan struct {
	Keep   map[string][]Destination
	Unused []string
	Retire map[int64][]Destination
}

// prepareReq is a deploy about to put an app on its egress network.
type prepareReq struct {
	app   int64
	dests []Destination
}

// appliedRec is the newest deployment of an app that recorded its link
// selection.
type appliedRec struct {
	id    int64
	dests []Destination
}

// servedRec is a name the sidecar answers for, and the app's newest
// deployment when it began.
type servedRec struct {
	dest  Destination
	floor int64
}

// planInput is what the sidecar's plan is made from.
type planInput struct {
	nets      []docker.PrefixNetwork
	saved     map[int64][]Destination
	applied   map[int64]appliedRec
	preparing map[int64][]Destination
	served    map[int64][]servedRec
	prep      *prepareReq
}

// planSidecar decides the sidecar's networks and names. A network stays while
// a service other than the sidecar attaches it or a task of one is on it, or
// while its app is being prepared for a deploy. On a network it stays on, the
// sidecar answers for what the app may still need: its saved selection, the
// one its last recorded deployment applied, the one its service's current
// spec carries, one being prepared, and every name it already answers for —
// a served name is retired only when it is in none of those, every live task
// of the app runs the current spec (converged) and a deployment newer than
// the one current when the name began has recorded its selection. Until then
// the app's connections to it are refused, never sent to the real address.
func planSidecar(names egressNames, in planInput) sidecarPlan {
	p := sidecarPlan{Keep: map[string][]Destination{}, Retire: map[int64][]Destination{}}
	for _, n := range in.nets {
		appID, ok := names.appOf(n.Name)
		if !ok {
			continue // not one of Krill's: leave it alone
		}
		isPrep := in.prep != nil && in.prep.app == appID
		pending, isPreparing := in.preparing[appID]
		if !usedByApps(n, names.sidecar) && !isPreparing && !isPrep {
			p.Unused = append(p.Unused, n.Name)
			continue
		}
		needed := unionDests(in.saved[appID], in.applied[appID].dests, labelSelection(n.Labels[names.appService(appID)]), pending)
		if isPrep {
			needed = unionDests(needed, in.prep.dests)
		}
		keep := needed
		done := converged(n, names.appService(appID))
		for _, s := range in.served[appID] {
			if slices.Contains(needed, s.dest) {
				continue
			}
			if done && in.applied[appID].id > s.floor {
				p.Retire[appID] = append(p.Retire[appID], s.dest)
				continue
			}
			keep = unionDests(keep, []Destination{s.dest})
		}
		p.Keep[n.Name] = keep
	}
	if in.prep != nil {
		name := names.network(in.prep.app)
		if _, ok := p.Keep[name]; !ok {
			var served []Destination
			for _, s := range in.served[in.prep.app] {
				served = append(served, s.dest)
			}
			p.Keep[name] = unionDests(in.saved[in.prep.app], in.applied[in.prep.app].dests, in.prep.dests, served)
		}
	}
	slices.Sort(p.Unused)
	return p
}

// converged reports whether every live task of svc on n runs the service's
// current spec: an old task still draining after a rolling update, or one
// left by a deploy Krill never recorded, may still need a name.
func converged(n docker.PrefixNetwork, svc string) bool {
	cur, attached := n.ForceUpdate[svc]
	for _, t := range n.Tasks {
		if t.Service != svc {
			continue
		}
		if !attached || t.ForceUpdate != cur {
			return false
		}
	}
	return true
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
