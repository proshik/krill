package server

import (
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

// clusterIPs returns every node's public IP (the control-plane advertise
// address plus every worker's ssh_host) — the allowlist the worker nftables
// ruleset trusts for cluster-scoped swarm traffic. Entries that don't parse as
// an IP (empty/unset config, a hostname instead of an IP, etc.) are skipped
// rather than fed into the generated nft script verbatim.
func (s *Server) clusterIPs(r *http.Request) ([]string, error) {
	rows, err := s.q.ListClusterNodes(r.Context())
	if err != nil {
		return nil, err
	}
	var ips []string
	if ip := net.ParseIP(s.cfg.AdvertiseAddr); ip != nil {
		ips = append(ips, ip.String())
	}
	for _, n := range rows {
		if ip := net.ParseIP(n.SshHost); ip != nil {
			ips = append(ips, ip.String())
		}
	}
	return ips, nil
}

// firewallRunner builds a Runner that reaches the given worker over SSH using
// its stored (encrypted) credentials.
func firewallRunner(n db.ClusterNode) firewall.RealRunner {
	return firewall.RealRunner{
		Spec: cluster.JoinSpec{
			Host:       n.SshHost,
			Port:       int(n.SshPort),
			User:       n.SshUser,
			PrivateKey: []byte(secret.Dec(n.SshKey)),
			HostKey:    n.HostKey,
		},
		Timeout: firewallRunnerTimeout,
	}
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
		s.flashErrT(w, r, "flash.err.internal")
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
		rr := firewallRunner(n)
		if err := firewall.Apply(r.Context(), rr, ruleset); err != nil {
			logFrom(r).Warn("firewall apply failed", "err", err, "node", n.Name)
			continue // dead-man switch on the node auto-reverts
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
		rr := firewallRunner(n)
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
