// Package firewall builds and applies an nftables allowlist to worker nodes so
// they expose nothing to the internet except SSH and cluster-scoped swarm ports.
package firewall

import (
	"errors"
	"fmt"
	"strings"
)

// BuildWorkerRuleset returns an `nft -f` script (dedicated table `inet krill`)
// that drops all inbound to the worker host except: established/related, loopback,
// ICMP, SSH (always, first — anti-lockout), and swarm/overlay ports from cluster
// peers only. clusterIPs must be non-empty (all node public IPs) — an empty set
// would drop swarm traffic and strand the worker. INPUT-only by design: container
// published ports (FORWARD path) are out of scope (see the design doc).
func BuildWorkerRuleset(clusterIPs []string) (string, error) {
	if len(clusterIPs) == 0 {
		return "", errors.New("firewall: empty cluster IP set")
	}
	ips := strings.Join(clusterIPs, ", ")
	var b strings.Builder
	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("table inet krill { }\n")        // ensure exists so delete never errors
	b.WriteString("delete table inet krill\n")     // idempotent full replace
	b.WriteString("table inet krill {\n")
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority 0; policy drop;\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    iif \"lo\" accept\n")
	b.WriteString("    tcp dport 22 accept\n") // SSH first — anti-lockout
	b.WriteString("    meta l4proto icmp accept\n")
	b.WriteString("    meta l4proto ipv6-icmp accept\n")
	b.WriteString(fmt.Sprintf("    ip saddr { %s } tcp dport { 2377, 7946 } accept\n", ips))
	b.WriteString(fmt.Sprintf("    ip saddr { %s } udp dport { 4789, 7946 } accept\n", ips))
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String(), nil
}
