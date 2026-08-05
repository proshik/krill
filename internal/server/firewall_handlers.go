package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/proshik/krill/internal/cluster"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

// firewallRunnerTimeout bounds each SSH command run against a worker during
// lockdown/open.
const firewallRunnerTimeout = 20 * time.Second

// errAdvertiseAddrNotIP is returned by clusterIPs when KRILL_ADVERTISE_ADDR is
// set but doesn't parse as an IP — locking down workers on an incomplete
// allowlist would drop the manager's swarm ports and strand the workers.
var errAdvertiseAddrNotIP = errors.New("advertise address does not parse as an IP")

// clusterIPs returns every node's public IP (the control-plane advertise
// address plus every worker's ssh_host) — the allowlist the worker nftables
// ruleset trusts for cluster-scoped swarm traffic. A worker's ssh_host given as
// a DNS name is resolved (an unresolvable one is a hard error: a node missing
// from the allowlist is a node cut off from the cluster), and a non-empty, unparsable
// AdvertiseAddr is a hard error: silently dropping the manager from the
// allowlist would leave the ruleset non-empty (workers still pass the
// empty-guard) while missing the one IP every worker needs to keep reaching
// the swarm — see errAdvertiseAddrNotIP.
func (s *Server) clusterIPs(r *http.Request) ([]string, error) {
	rows, err := s.q.ListClusterNodes(r.Context())
	if err != nil {
		return nil, err
	}
	var ips []string
	if s.cfg.AdvertiseAddr != "" {
		ip := net.ParseIP(s.cfg.AdvertiseAddr)
		if ip == nil {
			return nil, errAdvertiseAddrNotIP
		}
		ips = append(ips, ip.String())
	}
	for _, n := range rows {
		if ip := net.ParseIP(n.SshHost); ip != nil {
			ips = append(ips, ip.String())
			continue
		}
		// A node registered by DNS name used to be skipped silently — and a node
		// missing from the allowlist is a node cut off from swarm and overlay
		// traffic the moment the lockdown applies. Resolve it, and refuse the
		// lockdown outright if we cannot.
		rctx, cancel := context.WithTimeout(r.Context(), clusterIPResolveTimeout)
		addrs, rerr := net.DefaultResolver.LookupIPAddr(rctx, n.SshHost)
		cancel()
		if rerr != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("cluster node %q: cannot resolve %q to an IP (it would be locked out of the cluster): %w", n.Name, n.SshHost, rerr)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP.String())
		}
	}
	return ips, nil
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

// firewallPage renders the Network/Firewall page: the org's worker nodes and
// their current firewall_managed state, plus lockdown/open controls.
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
	render(w, r, http.StatusOK, templates.Firewall(o, role, rows))
}

// lockdownWorkers applies the nftables allowlist to every worker node with the
// dead-man switch, confirming on success. Instance-admin only. v1 scope:
// ListClusterNodes returns only worker rows (the control-plane isn't one), so
// this locks down workers only.
func (s *Server) lockdownWorkers(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	ips, err := s.clusterIPs(r)
	if err != nil {
		logFrom(r).Error("lockdownWorkers: list cluster IPs failed", "err", err)
		if errors.Is(err, errAdvertiseAddrNotIP) {
			s.flashErrT(w, r, "flash.err.firewall_advertise_not_ip")
		} else {
			s.flashErrT(w, r, "flash.err.internal")
		}
		return
	}
	ruleset, err := firewall.BuildWorkerRuleset(ips)
	if err != nil {
		s.flashErrT(w, r, "flash.err.firewall_no_nodes")
		return
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
		if !s.nodeSwarmReady(r.Context(), n.SwarmNodeID) {
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
	s.flashOK(w, r, "flash.ok.firewall_locked")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/firewall", http.StatusSeeOther)
}

// nodeSwarmReady polls the live swarm node list a few times over ~10-15s and
// reports whether swarmNodeID stays Ready/Active throughout — swarm takes a
// few seconds to notice a node whose overlay traffic just got cut off by a
// firewall change, so a single snapshot right after Apply isn't trustworthy.
// s.engine == nil (unit tests, or a single-node deployment with no cluster to
// check) returns true: the SSH-based Apply/Confirm round-trip already ran and
// there's no swarm to inspect.
func (s *Server) nodeSwarmReady(ctx context.Context, swarmNodeID string) bool {
	if s.engine == nil {
		return true
	}
	const checks = 3
	for i := 0; i < checks; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(4 * time.Second):
			}
		}
		nodes, err := s.engine.Nodes(ctx)
		if err != nil {
			return false
		}
		ready := false
		for _, n := range nodes {
			if n.ID == swarmNodeID {
				ready = n.State == "ready" && n.Availability == "active"
				break
			}
		}
		if !ready {
			return false
		}
	}
	return true
}

// openWorkers removes the lockdown table on every worker node. Instance-admin
// only.
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
	s.flashOK(w, r, "flash.ok.firewall_opened")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/firewall", http.StatusSeeOther)
}
