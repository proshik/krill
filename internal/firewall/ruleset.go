// Package firewall builds and applies an nftables allowlist to cluster nodes so
// they expose nothing to the internet except SSH, cluster-scoped swarm ports,
// and — on the control plane — the ports it must keep serving to the world.
package firewall

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sshPort is accepted unconditionally and first, as the anti-lockout rule; it is
// never repeated in the service-port set.
const sshPort = 22

// Rules describes one node's lockdown ruleset.
type Rules struct {
	// ClusterIPs are the node addresses trusted for swarm/overlay traffic. It
	// must be non-empty: an empty set would drop swarm traffic and strand the
	// node.
	ClusterIPs []string
	// PublicTCP are host-bound TCP ports kept open to the world (the manager's
	// ingress and UI ports). SSH is always open and is ignored if listed.
	PublicTCP []int
	// GatewayTCP are TCP ports admitted only from GatewayInterface.
	GatewayTCP []int
	// WireGuardUDP are the node's WireGuard listen ports. They are admitted
	// from anywhere: WireGuard drops every packet that does not authenticate,
	// and a peer's endpoint address is not necessarily a cluster address.
	WireGuardUDP []int
	// ClusterIface, when set, restricts swarm/overlay traffic to that
	// interface (the WireGuard interface the swarm runs over): VXLAN carries
	// no authentication of its own, so it must not be accepted from the
	// public interface even from a trusted source address.
	ClusterIface string
}

// BuildWorkerRuleset returns an `nft -f` script (dedicated table `inet krill`)
// that drops all inbound to the worker host except: established/related, loopback,
// ICMP, SSH (always, first — anti-lockout), and swarm/overlay ports from cluster
// peers only. clusterIPs must be non-empty (all node public IPs) — an empty set
// would drop swarm traffic and strand the worker. INPUT-only by design: container
// published ports (FORWARD path) are out of scope (see the design doc).
func BuildWorkerRuleset(clusterIPs []string) (string, error) {
	return Build(Rules{ClusterIPs: clusterIPs})
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
	return Build(Rules{ClusterIPs: clusterIPs, PublicTCP: servicePorts, GatewayTCP: gatewayPorts})
}

// markerPrefix names the empty, hookless chain every ruleset carries. Its
// suffix is a hash of the rest of the ruleset, so the live table (and the
// copy persisted for boot) says which ruleset it is without Krill having to
// compare nft's own listing format with the script it generated.
const markerPrefix = "krill_rs_"

var markerRe = regexp.MustCompile(`chain (` + markerPrefix + `[0-9a-f]{16})\b`)

// RulesetID returns the marker a ruleset — a generated script, a live
// `nft list table` or a persisted copy — carries, or "" when it has none (a
// table written by an older Krill or by hand).
func RulesetID(text string) string {
	m := markerRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

// ifaceRe is what an interface name may look like in the ruleset; anything
// else could break out of the quoted nft string.
var ifaceRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// Build returns the `nft -f` script for r. The script replaces the whole
// `inet krill` table in one transaction and is deterministic for a given r,
// so its marker identifies it.
func Build(r Rules) (string, error) {
	if len(r.ClusterIPs) == 0 {
		return "", errors.New("firewall: empty cluster IP set")
	}
	// nft's `ip saddr` matches IPv4 only. Putting an IPv6 address in it makes
	// the whole ruleset fail to load, the dead-man switch reverts, and the
	// lockdown silently never applies — so split by family and emit only the
	// families that actually have members.
	var v4, v6 []string
	seen := map[string]bool{}
	for _, raw := range r.ClusterIPs {
		ip := net.ParseIP(raw)
		if ip == nil {
			return "", fmt.Errorf("firewall: %q is not an IP address", raw)
		}
		k := ip.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		if ip.To4() != nil {
			v4 = append(v4, k)
		} else {
			v6 = append(v6, k)
		}
	}
	// Sorted so that the same cluster resolved in another order yields the
	// same ruleset, and with it the same marker.
	sort.Strings(v4)
	sort.Strings(v6)
	ports, err := normalizeServicePorts(r.PublicTCP)
	if err != nil {
		return "", err
	}
	gwPorts, err := normalizeServicePorts(r.GatewayTCP)
	if err != nil {
		return "", err
	}
	wgPorts, err := normalizePorts(r.WireGuardUDP)
	if err != nil {
		return "", err
	}
	if r.ClusterIface != "" && !ifaceRe.MatchString(r.ClusterIface) {
		return "", fmt.Errorf("firewall: invalid interface name %q", r.ClusterIface)
	}
	cluster := ""
	if r.ClusterIface != "" {
		cluster = fmt.Sprintf("iifname %q ", r.ClusterIface)
	}
	var b strings.Builder
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
	if len(wgPorts) > 0 {
		b.WriteString(fmt.Sprintf("    udp dport { %s } accept\n", joinPorts(wgPorts)))
	}
	b.WriteString("    meta l4proto icmp accept\n")
	b.WriteString("    meta l4proto ipv6-icmp accept\n")
	if len(v4) > 0 {
		ips := strings.Join(v4, ", ")
		b.WriteString(fmt.Sprintf("    %sip saddr { %s } tcp dport { 2377, 7946 } accept\n", cluster, ips))
		b.WriteString(fmt.Sprintf("    %sip saddr { %s } udp dport { 4789, 7946 } accept\n", cluster, ips))
	}
	if len(v6) > 0 {
		ips := strings.Join(v6, ", ")
		b.WriteString(fmt.Sprintf("    %sip6 saddr { %s } tcp dport { 2377, 7946 } accept\n", cluster, ips))
		b.WriteString(fmt.Sprintf("    %sip6 saddr { %s } udp dport { 4789, 7946 } accept\n", cluster, ips))
	}
	body := b.String()
	sum := sha256.Sum256([]byte(body))
	var out strings.Builder
	out.WriteString("#!/usr/sbin/nft -f\n")
	out.WriteString("table inet krill { }\n")    // ensure exists so delete never errors
	out.WriteString("delete table inet krill\n") // idempotent full replace
	out.WriteString("table inet krill {\n")
	out.WriteString("  chain " + markerPrefix + hex.EncodeToString(sum[:8]) + " {\n  }\n")
	out.WriteString("  chain input {\n")
	out.WriteString(body)
	out.WriteString("  }\n")
	out.WriteString("}\n")
	return out.String(), nil
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

// normalizePorts validates, de-duplicates and sorts ports without dropping
// any (unlike normalizeServicePorts, which skips 22 as already accepted).
func normalizePorts(ports []int) ([]int, error) {
	seen := make(map[int]struct{}, len(ports))
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("firewall: port %d out of range", p)
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
