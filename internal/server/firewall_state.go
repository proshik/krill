package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/web/templates"
)

// controlPlaneCheckKey keys the control plane in firewall_checks; workers are
// keyed by their cluster_nodes id.
const controlPlaneCheckKey = "control-plane"

func workerCheckKey(id int64) string { return "node-" + strconv.FormatInt(id, 10) }

// swarmAddrs returns the address each node runs the swarm over: the control
// plane's advertise address, and every swarm node's own address by node ID.
// all holds every one of them, for deciding whether the whole cluster sits on
// one WireGuard subnet. Lookup failures leave addresses out rather than
// failing: an unknown address only means the ruleset is not bound to an
// interface, which is the safe direction.
func (s *Server) swarmAddrs(ctx context.Context) (cp net.IP, byNode map[string]net.IP, all []net.IP) {
	byNode = map[string]net.IP{}
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if k := ip.String(); !seen[k] {
			seen[k] = true
			all = append(all, ip)
		}
	}
	if s.cfg.AdvertiseAddr != "" {
		if ips, err := resolveHostIPs(ctx, s.cfg.AdvertiseAddr); err == nil && len(ips) > 0 {
			cp = ips[0]
			for _, ip := range ips {
				if ip.To4() != nil {
					cp = ip
					break
				}
			}
			add(cp)
		}
	}
	if s.engine != nil {
		nodes, err := s.engine.Nodes(ctx)
		if err != nil {
			slog.Warn("firewall: list swarm nodes for their addresses failed", "err", err)
			return cp, byNode, all
		}
		for _, n := range nodes {
			if ip := net.ParseIP(n.Addr); ip != nil && !ip.IsUnspecified() {
				byNode[n.ID] = ip
				add(ip)
			}
		}
	}
	return cp, byNode, all
}

// nodeRuleset probes a node and builds the ruleset base needs on it.
func nodeRuleset(ctx context.Context, r firewall.Runner, addr net.IP, base firewall.Rules, all []net.IP) (string, firewall.Host, error) {
	h, err := firewall.Probe(ctx, r, addr)
	if err != nil {
		return "", h, err
	}
	if !h.Nft {
		return "", h, firewall.ErrNftMissing
	}
	rules, err := h.Rules(base, all)
	if err != nil {
		return "", h, err
	}
	rs, err := firewall.Build(rules)
	return rs, h, err
}

// probeErrKey maps a nodeRuleset failure to its flash key.
func probeErrKey(err error) string {
	switch {
	case errors.Is(err, firewall.ErrWireGuardPortsUnknown):
		return "flash.err.firewall_wg_ports_unknown"
	case errors.Is(err, firewall.ErrNftMissing):
		return "flash.err.firewall_nft_missing"
	default:
		return "flash.err.firewall_probe_failed"
	}
}

// classifyFirewall turns a probe into the page's state and drift notes.
// expectedID is the marker of the ruleset a lockdown would apply now ("" when
// it could not be computed); managed is whether Krill believes it locked the
// node (for the control plane, which has no row, a persisted ruleset is the
// only record of that intent).
func classifyFirewall(h firewall.Host, expectedID string, managed bool) (string, []string) {
	var drift []string
	state := "open"
	switch {
	case !h.Nft:
		return "nft_missing", nil
	case h.Pending:
		state = "pending"
	case h.Table:
		state = "locked"
	}
	if h.Table {
		switch {
		case h.LiveID == "":
			drift = append(drift, "firewall.drift.foreign")
		case expectedID != "" && h.LiveID != expectedID:
			drift = append(drift, "firewall.drift.outdated")
		}
		if !h.Pending && (!h.Persisted || h.PersistedID != h.LiveID || !h.UnitEnabled) {
			drift = append(drift, "firewall.drift.not_persisted")
		}
	} else if managed || h.Persisted {
		state = "missing"
		drift = append(drift, "firewall.drift.missing")
	}
	return state, drift
}

// firewallNodes reads every node's live state for the Firewall page, in
// parallel and bounded by firewallStatusTimeout per node.
func (s *Server) firewallNodes(r *http.Request, rows []db.ClusterNode) []templates.FirewallNode {
	ctx := r.Context()
	cpAddr, byNode, all := s.swarmAddrs(ctx)
	ips, ipErr := s.clusterIPs(r)
	panelRow, panelErr := s.panelRow(ctx)
	checks := map[string]time.Time{}
	if list, err := s.q.ListFirewallChecks(ctx); err == nil {
		for _, c := range list {
			checks[c.Node] = c.CheckedAt
		}
	} else {
		slog.Warn("firewall page: read last checks failed", "err", err)
	}

	type job struct {
		key     string
		name    string
		runner  firewall.Runner
		addr    net.IP
		base    firewall.Rules
		baseOK  bool
		managed bool
	}
	var jobs []job
	cp := job{key: controlPlaneCheckKey, name: "control-plane", runner: s.cpFirewall, addr: cpAddr}
	if ipErr == nil && panelErr == nil {
		if ports, err := controlPlaneServicePorts(s.cfg.ListenAddr, panelRow.DirectPortClosed); err == nil {
			cp.base = firewall.Rules{ClusterIPs: ips, PublicTCP: ports.Public, GatewayTCP: ports.Gateway}
			cp.baseOK = true
		}
	}
	jobs = append(jobs, cp)
	for _, n := range rows {
		j := job{key: workerCheckKey(n.ID), name: n.Name, addr: byNode[n.SwarmNodeID], managed: n.FirewallManaged}
		if rr, err := firewallRunner(n); err == nil {
			j.runner = rr
		}
		if ipErr == nil {
			j.base = firewall.Rules{ClusterIPs: ips}
			j.baseOK = true
		}
		jobs = append(jobs, j)
	}

	out := make([]templates.FirewallNode, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		out[i] = templates.FirewallNode{Name: j.name, ControlPlane: j.key == controlPlaneCheckKey, Managed: j.managed}
		if t, ok := checks[j.key]; ok {
			out[i].LastOK = t
		}
		if j.runner == nil {
			out[i].State = "unavailable"
			continue
		}
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, firewallStatusTimeout)
			defer cancel()
			h, err := firewall.Probe(pctx, j.runner, j.addr)
			if err != nil {
				slog.Warn("firewall page: probe failed", "node", j.name, "err", err)
				out[i].State = "unavailable"
				return
			}
			expected := ""
			if j.baseOK {
				if rules, err := h.Rules(j.base, all); err == nil {
					if rs, err := firewall.Build(rules); err == nil {
						expected = firewall.RulesetID(rs)
					}
				} else {
					out[i].Drift = append(out[i].Drift, "firewall.drift.wg_ports_unknown")
				}
			}
			managed := j.managed
			state, drift := classifyFirewall(h, expected, managed)
			out[i].State = state
			out[i].Drift = append(out[i].Drift, drift...)
			if h.SwarmIface != nil {
				out[i].Iface = h.SwarmIface.Name
				out[i].MTU = h.SwarmIface.MTU
				out[i].WireGuard = h.SwarmIface.WireGuard
			}
			out[i].WireGuardPorts = h.WireGuardPorts
			out[i].LastOK = time.Now()
			if err := s.q.RecordFirewallCheck(context.WithoutCancel(ctx), j.key); err != nil {
				slog.Warn("firewall page: record check failed", "node", j.name, "err", err)
			}
		}(i, j)
	}
	wg.Wait()
	return out
}

// overlayMTUView compares the overlay MTU Krill creates networks with, the
// smallest swarm-interface MTU the probes found, and the MTU each existing
// Krill network actually has. mtus is nil when the engine cannot report it.
func overlayMTUView(configured int, base string, nodes []templates.FirewallNode, mtus map[string]int) templates.OverlayMTU {
	v := templates.OverlayMTU{Configured: configured, Effective: configured}
	if v.Effective == 0 {
		v.Effective = docker.DefaultOverlayMTU
	}
	for _, n := range nodes {
		if n.MTU > 0 && (v.MinIface == 0 || n.MTU < v.MinIface) {
			v.MinIface, v.MinIfaceNode, v.MinIfaceName = n.MTU, n.Name, n.Iface
		}
	}
	if v.MinIface > 0 {
		v.Recommended = v.MinIface - vxlanOverhead
		v.TooLarge = v.Effective > v.Recommended
	}
	v.Available = mtus != nil
	names := make([]string, 0, len(mtus))
	for name := range mtus {
		if name == base || strings.HasPrefix(name, "krill-") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		mtu := mtus[name]
		v.Networks = append(v.Networks, templates.NetworkMTU{
			Name:     name,
			MTU:      mtu,
			Differs:  mtu != v.Effective,
			TooLarge: v.Recommended > 0 && mtu > v.Recommended,
		})
	}
	return v
}

// vxlanOverhead is what VXLAN adds to an overlay packet on the underlay.
const vxlanOverhead = 50

// overlayMTUs reads every overlay network's MTU, or nil when the engine cannot.
func (s *Server) overlayMTUs(ctx context.Context) map[string]int {
	ov, ok := s.engine.(docker.OverlayNetworks)
	if !ok {
		return nil
	}
	mtus, err := ov.OverlayMTUs(ctx)
	if err != nil {
		slog.Warn("firewall page: read overlay network MTUs failed", "err", err)
		return nil
	}
	return mtus
}
