// Package firewall builds and applies an nftables allowlist to cluster nodes so
// they expose nothing to the internet except SSH, cluster-scoped swarm ports,
// and — on the control plane — the ports it must keep serving to the world.
package firewall

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// sshPort is accepted unconditionally and first, as the anti-lockout rule; it is
// never repeated in the service-port set.
const sshPort = 22

// BuildWorkerRuleset returns an `nft -f` script (dedicated table `inet krill`)
// that drops all inbound to the worker host except: established/related, loopback,
// ICMP, SSH (always, first — anti-lockout), and swarm/overlay ports from cluster
// peers only. clusterIPs must be non-empty (all node public IPs) — an empty set
// would drop swarm traffic and strand the worker. INPUT-only by design: container
// published ports (FORWARD path) are out of scope (see the design doc).
func BuildWorkerRuleset(clusterIPs []string) (string, error) {
	return buildRuleset(clusterIPs, nil, nil)
}

// GatewayInterface is the host-side bridge Swarm tasks leave through to reach
// anything outside their overlay networks, the host's own addresses included.
// A request the Traefik gateway proxies to a listener on the host arrives on
// it.
const GatewayInterface = "docker_gwbridge"

// BuildManagerRuleset returns the same allowlist for the control-plane host,
// plus servicePorts — the host-bound TCP ports the manager must keep answering
// on. Locking the manager down with the worker ruleset would drop them: the
// Krill UI (KRILL_LISTEN_ADDR) and the ingress listeners go silent, the operator
// loses the very page the lockdown was triggered from, and the only way back is
// the dead-man switch. Callers pass the ingress ports and the UI port; 22 is
// already covered and is ignored if passed.
//
// Ports published by containers are DNAT'd and traverse FORWARD rather than
// INPUT, so they are unaffected either way; servicePorts is about listeners
// bound on the host itself, and about stating the intent explicitly rather than
// relying on that distinction holding.
//
// gatewayPorts are accepted only when they arrive on GatewayInterface, i.e.
// from a Swarm task on this host: the Krill UI port once the panel is served
// through the gateway on its domain and closed to the outside world.
func BuildManagerRuleset(clusterIPs []string, servicePorts, gatewayPorts []int) (string, error) {
	return buildRuleset(clusterIPs, servicePorts, gatewayPorts)
}

// buildRuleset is the shared body: identical for both roles except for the
// extra accepted service ports the manager needs.
func buildRuleset(clusterIPs []string, servicePorts, gatewayPorts []int) (string, error) {
	if len(clusterIPs) == 0 {
		return "", errors.New("firewall: empty cluster IP set")
	}
	// nft's `ip saddr` matches IPv4 only. Putting an IPv6 address in it makes
	// the whole ruleset fail to load, the dead-man switch reverts, and the
	// lockdown silently never applies — so split by family and emit only the
	// families that actually have members.
	var v4, v6 []string
	for _, raw := range clusterIPs {
		ip := net.ParseIP(raw)
		if ip == nil {
			return "", fmt.Errorf("firewall: %q is not an IP address", raw)
		}
		if ip.To4() != nil {
			v4 = append(v4, ip.String())
		} else {
			v6 = append(v6, ip.String())
		}
	}
	ports, err := normalizeServicePorts(servicePorts)
	if err != nil {
		return "", err
	}
	gwPorts, err := normalizeServicePorts(gatewayPorts)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("table inet krill { }\n")    // ensure exists so delete never errors
	b.WriteString("delete table inet krill\n") // idempotent full replace
	b.WriteString("table inet krill {\n")
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority 0; policy drop;\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    iif \"lo\" accept\n")
	b.WriteString("    tcp dport 22 accept\n") // SSH first — anti-lockout
	if len(ports) > 0 {
		b.WriteString(fmt.Sprintf("    tcp dport { %s } accept\n", joinPorts(ports)))
	}
	if len(gwPorts) > 0 {
		b.WriteString(fmt.Sprintf("    iifname %q tcp dport { %s } accept\n", GatewayInterface, joinPorts(gwPorts)))
	}
	b.WriteString("    meta l4proto icmp accept\n")
	b.WriteString("    meta l4proto ipv6-icmp accept\n")
	if len(v4) > 0 {
		ips := strings.Join(v4, ", ")
		b.WriteString(fmt.Sprintf("    ip saddr { %s } tcp dport { 2377, 7946 } accept\n", ips))
		b.WriteString(fmt.Sprintf("    ip saddr { %s } udp dport { 4789, 7946 } accept\n", ips))
	}
	if len(v6) > 0 {
		ips := strings.Join(v6, ", ")
		b.WriteString(fmt.Sprintf("    ip6 saddr { %s } tcp dport { 2377, 7946 } accept\n", ips))
		b.WriteString(fmt.Sprintf("    ip6 saddr { %s } udp dport { 4789, 7946 } accept\n", ips))
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// normalizeServicePorts validates, de-duplicates and sorts the extra accepted
// ports. Sorting keeps the emitted ruleset deterministic for a given input, so
// a re-apply that changes nothing produces a byte-identical script. An
// out-of-range port is a hard error: nft would reject the whole ruleset, the
// dead-man switch would revert, and the lockdown would silently never apply —
// the same failure the address-family split exists to avoid.
func normalizeServicePorts(ports []int) ([]int, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	seen := make(map[int]struct{}, len(ports))
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("firewall: service port %d out of range", p)
		}
		if p == sshPort {
			continue // already accepted unconditionally above
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}

func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}
