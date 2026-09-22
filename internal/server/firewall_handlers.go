package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/proshik/krill/internal/cluster"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// firewallRunnerTimeout bounds each command run against a node (over SSH for a
// worker, locally for the control plane) during lockdown/open.
const firewallRunnerTimeout = 20 * time.Second

// errAdvertiseAddrUnset is returned by clusterIPs when KRILL_ADVERTISE_ADDR is
// empty. The ruleset's own empty-set guard cannot catch this: with workers
// registered the allowlist is non-empty, it just lacks the manager — and a
// lockdown on that list cuts every worker off from the manager's swarm ports.
var errAdvertiseAddrUnset = errors.New("advertise address is not set")

// errAdvertiseAddrUnresolvable is returned by clusterIPs when
// KRILL_ADVERTISE_ADDR is neither an IP nor a name that resolves to one.
var errAdvertiseAddrUnresolvable = errors.New("advertise address does not resolve to an IP")

// clusterIPs returns the allowlist the nftables rulesets trust for
// cluster-scoped swarm traffic: the control-plane advertise address, every
// worker's ssh_host, and the address swarm itself reports for every node.
//
// The advertise address and a worker's ssh_host may be DNS names; both are
// resolved the same way (a `docker swarm join` works with a name, so refusing
// one here would let nodes join a cluster they could then not lock down). An
// unresolvable name is a hard error: a node missing from the allowlist is a
// node cut off from the cluster. The swarm-reported addresses matter when swarm
// runs over a private overlay such as WireGuard — the peers' swarm traffic then
// comes from their tunnel addresses, not from the public ssh_host.
func (s *Server) clusterIPs(r *http.Request) ([]string, error) {
	ctx := r.Context()
	rows, err := s.q.ListClusterNodes(ctx)
	if err != nil {
		return nil, err
	}
	if s.cfg.AdvertiseAddr == "" {
		return nil, errAdvertiseAddrUnset
	}
	var ips []string
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if k := ip.String(); !seen[k] {
			seen[k] = true
			ips = append(ips, k)
		}
	}
	addrs, err := resolveHostIPs(ctx, s.cfg.AdvertiseAddr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", errAdvertiseAddrUnresolvable, s.cfg.AdvertiseAddr, err)
	}
	for _, ip := range addrs {
		add(ip)
	}
	for _, n := range rows {
		addrs, err := resolveHostIPs(ctx, n.SshHost)
		if err != nil {
			return nil, fmt.Errorf("cluster node %q: cannot resolve %q to an IP (it would be locked out of the cluster): %w", n.Name, n.SshHost, err)
		}
		for _, ip := range addrs {
			add(ip)
		}
	}
	if s.engine != nil {
		nodes, err := s.engine.Nodes(ctx)
		if err != nil {
			return nil, fmt.Errorf("list swarm nodes: %w", err)
		}
		for _, n := range nodes {
			// A manager may report 0.0.0.0; it carries no usable source address.
			if ip := net.ParseIP(n.Addr); ip != nil && !ip.IsUnspecified() {
				add(ip)
			}
		}
	}
	return ips, nil
}

// resolveHostIPs returns host itself when it is an IP literal, otherwise the
// addresses it resolves to (bounded by clusterIPResolveTimeout).
func resolveHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	rctx, cancel := context.WithTimeout(ctx, clusterIPResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses for %q", host)
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// clusterIPResolveTimeout bounds one DNS lookup while assembling the allowlist.
const clusterIPResolveTimeout = 5 * time.Second

// firewallRunner builds the SSH runner for a node. An undecryptable key is
// reported rather than passed on empty: firewall Apply/Open would fail deep in
// the SSH handshake with an error that says nothing about the encryption key.
func firewallRunner(n db.ClusterNode) (firewall.RealRunner, error) {
	key, err := secret.Dec(n.SshKey)
	if err != nil {
		return firewall.RealRunner{}, fmt.Errorf("cluster node %d ssh key: %w", n.ID, err)
	}
	return firewall.RealRunner{
		Spec: cluster.JoinSpec{
			Host:       n.SshHost,
			Port:       int(n.SshPort),
			User:       n.SshUser,
			PrivateKey: []byte(key),
			HostKey:    n.HostKey,
		},
		Timeout: firewallRunnerTimeout,
	}, nil
}

// controlPlanePorts are the host-bound TCP ports the control plane keeps
// serving under lockdown.
type controlPlanePorts struct {
	Public  []int // accepted from anywhere
	Gateway []int // accepted only from the gateway (firewall.GatewayInterface)
}

// controlPlaneServicePorts returns the ports the control plane must keep
// serving under lockdown: HTTP/HTTPS ingress, and the Krill UI port from
// listenAddr (":8080", "0.0.0.0:8080", "[::]:8080"). An unparsable listen
// address is an error rather than a silent omission — a ruleset without the UI
// port locks the operator out of the page the lockdown was started from.
//
// closeDirect moves the UI port from public to gateway-only: the operator has
// confirmed the panel domain and chosen to stop serving the UI in plain HTTP
// to the world. The gateway itself still needs the port — it proxies the panel
// and polls the panel's routes there — so the port is narrowed, not dropped.
func controlPlaneServicePorts(listenAddr string, closeDirect bool) (controlPlanePorts, error) {
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return controlPlanePorts{}, fmt.Errorf("parse listen address %q: %w", listenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return controlPlanePorts{}, fmt.Errorf("listen address %q: invalid port %q", listenAddr, portStr)
	}
	if closeDirect {
		return controlPlanePorts{Public: []int{80, 443}, Gateway: []int{port}}, nil
	}
	return controlPlanePorts{Public: []int{80, 443, port}}, nil
}

// controlPlaneRuleset builds the control plane's lockdown ruleset, returning an
// i18n error key on failure.
func (s *Server) controlPlaneRuleset(r *http.Request, closeDirect bool) (string, string) {
	ips, err := s.clusterIPs(r)
	if err != nil {
		logFrom(r).Error("control-plane ruleset: list cluster IPs failed", "err", err)
		switch {
		case errors.Is(err, errAdvertiseAddrUnset):
			return "", "flash.err.firewall_advertise_unset"
		case errors.Is(err, errAdvertiseAddrUnresolvable):
			return "", "flash.err.firewall_advertise_not_ip"
		default:
			return "", "flash.err.internal"
		}
	}
	ports, err := controlPlaneServicePorts(s.cfg.ListenAddr, closeDirect)
	if err != nil {
		logFrom(r).Error("control-plane ruleset: listen address unusable", "err", err)
		return "", "flash.err.internal"
	}
	ruleset, err := firewall.BuildManagerRuleset(ips, ports.Public, ports.Gateway)
	if err != nil {
		logFrom(r).Error("control-plane ruleset: build failed", "err", err)
		return "", "flash.err.internal"
	}
	return ruleset, ""
}

// SetControlPlaneFirewall wires the runner that applies the firewall on the
// control-plane host itself (firewall.LocalRunner in production). Nil — tests,
// or a build that doesn't want it — leaves the control plane out of lockdown
// and shows its status as unavailable.
func (s *Server) SetControlPlaneFirewall(r firewall.Runner) { s.cpFirewall = r }

// controlPlaneFirewallState reads the live lockdown state of the control plane.
// It has no cluster_nodes row, so there is nothing persisted to fall back on.
func (s *Server) controlPlaneFirewallState(ctx context.Context) templates.ControlPlaneFirewall {
	if s.cpFirewall == nil {
		return templates.ControlPlaneFirewall{}
	}
	ctx, cancel := context.WithTimeout(ctx, firewallStatusTimeout)
	defer cancel()
	locked, err := firewall.Status(ctx, s.cpFirewall)
	if err != nil {
		slog.Warn("control-plane firewall status unavailable", "err", err)
		return templates.ControlPlaneFirewall{}
	}
	st := templates.ControlPlaneFirewall{Available: true, Locked: locked}
	if locked {
		pending, err := firewall.RevertPending(ctx, s.cpFirewall)
		if err != nil {
			slog.Warn("control-plane firewall revert state unavailable", "err", err)
		}
		st.Pending = pending
	}
	return st
}

// firewallStatusTimeout bounds the live status read on page render.
const firewallStatusTimeout = 5 * time.Second

// firewallPage renders the Network/Firewall page: the control plane's live
// firewall state, the worker nodes with their firewall_managed state, and the
// lockdown/open/confirm controls.
func (s *Server) firewallPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	rows, err := s.q.ListClusterNodes(r.Context())
	if err != nil {
		logFrom(r).Error("firewallPage: list cluster nodes failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cp := s.controlPlaneFirewallState(r.Context())
	if cp.Pending {
		// The confirmation must arrive on a connection opened AFTER the ruleset
		// was applied, or it proves nothing — see confirmControlPlaneFirewall.
		w.Header().Set("Connection", "close")
	}
	render(w, r, http.StatusOK, templates.Firewall(o, role, rows, cp))
}

// lockdownWorkers applies the nftables allowlist to every worker node, then to
// the control plane, each with the dead-man switch. Instance-admin only.
//
// Workers are confirmed here once they are verified swarm-Ready. The control
// plane is not: whether the operator can still reach the UI can only be proven
// by the operator's browser, so its switch stays armed until
// confirmControlPlaneFirewall receives a request over a fresh connection.
func (s *Server) lockdownWorkers(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	back := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/firewall"
	ips, err := s.clusterIPs(r)
	if err != nil {
		logFrom(r).Error("lockdownWorkers: list cluster IPs failed", "err", err)
		switch {
		case errors.Is(err, errAdvertiseAddrUnset):
			s.flashErrT(w, r, "flash.err.firewall_advertise_unset")
		case errors.Is(err, errAdvertiseAddrUnresolvable):
			s.flashErrT(w, r, "flash.err.firewall_advertise_not_ip")
		default:
			s.flashErrT(w, r, "flash.err.internal")
		}
		return
	}
	ruleset, err := firewall.BuildWorkerRuleset(ips)
	if err != nil {
		s.flashErrT(w, r, "flash.err.firewall_no_nodes")
		return
	}
	// Build the control-plane ruleset before touching any node, so a config
	// problem refuses the whole lockdown instead of leaving it half-applied.
	var cpRuleset string
	var panelRow db.PanelGateway
	if s.cpFirewall != nil {
		panelRow, err = s.panelRow(r.Context())
		if err != nil {
			logFrom(r).Error("lockdownWorkers: read panel settings failed", "err", err)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		// With the UI port closed to the world, this lockdown closes it again —
		// and its confirmation can only come back through the panel domain. Run
		// from anywhere else, the operator would watch it revert.
		if panelRow.DirectPortClosed && !s.onPanelDomain(r, panelRow) {
			s.flashErrT(w, r, "flash.err.firewall_use_panel_domain")
			return
		}
		var key string
		if cpRuleset, key = s.controlPlaneRuleset(r, panelRow.DirectPortClosed); key != "" {
			s.flashErrT(w, r, key)
			return
		}
	}
	rows, err := s.q.ListClusterNodes(r.Context())
	if err != nil {
		logFrom(r).Error("lockdownWorkers: list cluster nodes failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	for _, n := range rows {
		rr, kerr := firewallRunner(n)
		if kerr != nil {
			logFrom(r).Error("firewall lockdown: node ssh key unusable", "err", kerr, "node", n.Name)
			continue
		}
		if err := firewall.Apply(r.Context(), rr, ruleset); err != nil {
			logFrom(r).Warn("firewall apply failed", "err", err, "node", n.Name)
			continue // dead-man switch on the node auto-reverts
		}
		// SSH (dport 22) is always allowed by the ruleset, so Confirm alone
		// can't tell a healthy worker from one whose overlay/swarm traffic
		// just got cut off. Verify the node is still swarm-Ready before
		// cancelling the auto-revert — if it isn't, leave the dead-man
		// switch armed so the node reverts itself.
		if !s.nodesSwarmReady(r.Context(), logFrom(r), []string{n.SwarmNodeID}) {
			logFrom(r).Warn("firewall lockdown: node not swarm-ready after apply; skipping confirm, auto-revert will fire", "node", n.Name)
			continue
		}
		if err := firewall.Confirm(r.Context(), rr); err != nil {
			logFrom(r).Warn("firewall confirm failed", "err", err, "node", n.Name)
			continue
		}
		if err := s.q.SetClusterNodeFirewallManaged(r.Context(), db.SetClusterNodeFirewallManagedParams{ID: n.ID, FirewallManaged: true}); err != nil {
			logFrom(r).Error("lockdownWorkers: persist firewall_managed failed", "err", err, "node", n.Name)
		}
	}
	if s.cpFirewall == nil {
		s.flashOK(w, r, "flash.ok.firewall_locked")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if panelRow.DirectPortClosePending {
		// A panel close still awaiting its confirmation is superseded by this
		// ruleset, which was built from the confirmed state.
		if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{DirectPortClosed: panelRow.DirectPortClosed}); err != nil {
			logFrom(r).Error("lockdownWorkers: clear superseded panel close failed", "err", err)
		}
	}
	if res := s.lockdownControlPlane(r, cpRuleset); res.Err != "" {
		s.setFlash(w, r, "err", res.message(r.Context(), ""))
	} else {
		s.flashOK(w, r, "flash.ok.firewall_cp_pending")
	}
	// Close this connection so the browser reaches the redirect — and later the
	// confirmation — over new connections that the fresh ruleset has to admit.
	w.Header().Set("Connection", "close")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// cpApply is the outcome of lockdownControlPlane.
type cpApply struct {
	// Applied: the ruleset is on the host and the dead-man switch is armed,
	// whether or not the swarm check afterwards passed.
	Applied bool
	// Err is an i18n key; "" when the ruleset is in place and the swarm stayed
	// healthy. With Applied it is the swarm-check failure: the operator can
	// still confirm, and otherwise the switch reverts at RevertAt.
	Err string
	// RevertAt is when the armed switch fires: this apply's own, or the
	// earlier unconfirmed one that refused it (Err = firewall_cp_armed).
	RevertAt time.Time
}

// lockdownControlPlane applies ruleset to the control-plane host. It never
// confirms: see confirmControlPlaneFirewall.
func (s *Server) lockdownControlPlane(r *http.Request, ruleset string) cpApply {
	log := logFrom(r)
	// Snapshot which nodes are healthy BEFORE applying: a node that was already
	// down must not make the check fail, and one that goes down right after the
	// apply is the manager dropping its swarm traffic.
	healthy, err := s.readySwarmNodeIDs(r.Context())
	if err != nil {
		log.Error("control-plane firewall: list swarm nodes failed", "err", err)
		return cpApply{Err: "flash.err.internal"}
	}
	if err := firewall.Apply(r.Context(), s.cpFirewall, ruleset); err != nil {
		var armed *firewall.RevertArmedError
		if errors.As(err, &armed) {
			// An earlier change is unconfirmed; applying on top would replace
			// the snapshot its switch restores.
			log.Warn("control-plane firewall: an earlier change is still armed; nothing applied", "revert_at", armed.At)
			return cpApply{Err: "flash.err.firewall_cp_armed", RevertAt: armed.At}
		}
		log.Warn("control-plane firewall apply failed", "err", err)
		return cpApply{Err: "flash.err.firewall_cp_apply"}
	}
	res := cpApply{Applied: true, RevertAt: time.Now().Add(firewall.RevertDelay)}
	// Established connections pass any ruleset, so a keep-alive connection
	// opened before the apply could carry the confirmation past a rule that
	// admits nothing new. Drop the idle ones; the caller closes its own.
	if s.panel.closeIdleConns != nil {
		s.panel.closeIdleConns()
	}
	if !s.nodesSwarmReady(r.Context(), log, healthy) {
		log.Warn("control-plane firewall: swarm nodes dropped after apply; leaving the auto-revert armed", "revert_at", res.RevertAt)
		res.Err = "flash.err.firewall_cp_cluster"
		return res
	}
	log.Info("control-plane firewall applied; awaiting operator confirmation")
	return res
}

// message renders res.Err for a flash; key overrides it (a caller with its own
// wording for the same outcome). The keys for an armed switch take its time.
func (res cpApply) message(ctx context.Context, key string) string {
	if key == "" {
		key = res.Err
	}
	switch res.Err {
	case "flash.err.firewall_cp_armed", "flash.err.firewall_cp_cluster":
		at := "?"
		if !res.RevertAt.IsZero() {
			at = res.RevertAt.Local().Format("15:04:05")
		}
		return i18n.Tf(ctx, key, at)
	}
	return i18n.T(ctx, key)
}

// confirmControlPlaneFirewall cancels the control plane's dead-man switch.
// Instance-admin only.
//
// A health probe from this process cannot stand in for this request: the
// host's traffic to its own addresses enters through the loopback interface,
// which the ruleset always accepts, so a self-probe succeeds even when the
// operator has been locked out. Reaching this handler at all is the proof —
// the lockdown response and the page carrying this form both close their
// connection, so this request arrives over a connection the new ruleset had to
// admit. (A keep-alive connection another tab opened before the apply could
// still carry it; that residue is accepted.)
func (s *Server) confirmControlPlaneFirewall(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.cpFirewall == nil {
		http.NotFound(w, r)
		return
	}
	back := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/firewall"
	pending, err := firewall.RevertPending(r.Context(), s.cpFirewall)
	if err != nil {
		logFrom(r).Error("confirm control-plane firewall: read revert state failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if !pending {
		// Too late: the switch already fired and restored the previous ruleset.
		s.flashErrT(w, r, "flash.err.firewall_cp_not_pending")
		return
	}
	if err := firewall.Confirm(r.Context(), s.cpFirewall); err != nil {
		logFrom(r).Error("confirm control-plane firewall failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	// The switch is shared: confirming here also keeps a panel close the
	// operator applied from the panel page.
	if row, err := s.panelRow(r.Context()); err != nil {
		logFrom(r).Error("confirm control-plane firewall: read panel settings failed", "err", err)
	} else if row.DirectPortClosePending {
		if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{DirectPortClosed: true}); err != nil {
			logFrom(r).Error("confirm control-plane firewall: save panel close failed", "err", err)
		}
	}
	logFrom(r).Info("control-plane firewall confirmed")
	s.flashOK(w, r, "flash.ok.firewall_cp_confirmed")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// readySwarmNodeIDs returns the IDs of swarm nodes that are currently Ready and
// Active. s.engine == nil returns none (nothing to watch).
func (s *Server) readySwarmNodeIDs(ctx context.Context) ([]string, error) {
	if s.engine == nil {
		return nil, nil
	}
	nodes, err := s.engine.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, n := range nodes {
		if n.State == "ready" && n.Availability == "active" {
			ids = append(ids, n.ID)
		}
	}
	return ids, nil
}

// nodesSwarmReady polls the live swarm node list a few times over ~10-15s and
// reports whether every node in ids stays Ready/Active throughout — swarm
// takes a few seconds to notice a node whose overlay traffic just got cut off
// by a firewall change, so a single snapshot right after Apply isn't
// trustworthy. s.engine == nil (unit tests, or a single-node deployment with no
// cluster to check) or an empty ids returns true: there's no swarm to inspect.
//
// Only a node seen not Ready/Active fails the check. A failed node listing is
// the local daemon's answer, not evidence about the firewall, so it is logged
// and the next poll decides; it fails the check only if no poll succeeds.
func (s *Server) nodesSwarmReady(ctx context.Context, log *slog.Logger, ids []string) bool {
	if s.engine == nil || len(ids) == 0 {
		return true
	}
	const checks = 3
	seen := false
	for i := 0; i < checks; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				log.Warn("swarm readiness check: request ended before the check finished", "err", ctx.Err(), "poll", i+1)
				return false
			case <-time.After(4 * time.Second):
			}
		}
		nodes, err := s.engine.Nodes(ctx)
		if err != nil {
			log.Warn("swarm readiness check: listing nodes failed", "err", err, "poll", i+1)
			continue
		}
		seen = true
		byID := make(map[string]docker.SwarmNode, len(nodes))
		for _, n := range nodes {
			byID[n.ID] = n
		}
		for _, id := range ids {
			n, ok := byID[id]
			if !ok {
				log.Warn("swarm readiness check: node missing from the swarm", "node_id", id, "poll", i+1)
				return false
			}
			if n.State != "ready" || n.Availability != "active" {
				log.Warn("swarm readiness check: node not ready", "node_id", id, "hostname", n.Hostname,
					"state", n.State, "availability", n.Availability, "poll", i+1)
				return false
			}
		}
	}
	if !seen {
		log.Warn("swarm readiness check: no poll could list the nodes")
	}
	return seen
}

// openWorkers removes the lockdown table on every worker node and on the
// control plane. Instance-admin only.
func (s *Server) openWorkers(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	rows, err := s.q.ListClusterNodes(r.Context())
	if err != nil {
		logFrom(r).Error("openWorkers: list cluster nodes failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	for _, n := range rows {
		rr, kerr := firewallRunner(n)
		if kerr != nil {
			logFrom(r).Error("firewall open: node ssh key unusable", "err", kerr, "node", n.Name)
			continue
		}
		if err := firewall.Open(r.Context(), rr); err != nil {
			logFrom(r).Warn("firewall open failed", "err", err, "node", n.Name)
			continue
		}
		if err := s.q.SetClusterNodeFirewallManaged(r.Context(), db.SetClusterNodeFirewallManagedParams{ID: n.ID, FirewallManaged: false}); err != nil {
			logFrom(r).Error("openWorkers: persist firewall_managed failed", "err", err, "node", n.Name)
		}
	}
	if s.cpFirewall != nil {
		if err := firewall.Open(r.Context(), s.cpFirewall); err != nil {
			logFrom(r).Warn("control-plane firewall open failed", "err", err)
		} else if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{}); err != nil {
			// With the table gone the UI port is open again; record it, so a
			// later lockdown does not close it behind the operator's back.
			logFrom(r).Error("openWorkers: reset panel direct port failed", "err", err)
		}
	}
	s.flashOK(w, r, "flash.ok.firewall_opened")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/firewall", http.StatusSeeOther)
}
