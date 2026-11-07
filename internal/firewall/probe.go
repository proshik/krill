package firewall

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Host is what Probe reads from a node in one round trip: the live state of
// the lockdown and its boot persistence, and the node's network facts the
// ruleset (and the overlay MTU budget) depend on.
type Host struct {
	Nft         bool   // the nft binary is installed
	Table       bool   // the inet krill table is loaded
	LiveID      string // marker of the loaded table ("" without one)
	Pending     bool   // the dead-man switch is armed
	PersistedID string // marker of the ruleset persisted for boot ("" when none)
	Persisted   bool   // a persisted ruleset file exists
	UnitEnabled bool   // the boot unit that loads it is enabled

	// SwarmIface is the interface holding the node's swarm address, or nil
	// when the address was not found among the node's interfaces.
	SwarmIface *Iface
	// WireGuard lists the node's WireGuard interfaces.
	WireGuard []string
	// WireGuardPorts are the listen ports of those interfaces, when they could
	// be read (wireguard-tools installed). WireGuardPortsKnown is false when
	// the node has WireGuard interfaces but no way to read their ports.
	WireGuardPorts      []int
	WireGuardPortsKnown bool
}

// Iface is one network interface as the probe saw it.
type Iface struct {
	Name      string
	MTU       int
	Net       *net.IPNet // the address/prefix that matched
	WireGuard bool
}

// ErrWireGuardPortsUnknown is returned by Host.Rules for a node that has
// WireGuard interfaces whose listen ports cannot be read: a lockdown would drop
// WireGuard and with it everything that runs over it.
var ErrWireGuardPortsUnknown = errors.New("firewall: node has WireGuard interfaces but their listen ports cannot be read (install wireguard-tools)")

// Persistence paths. The ruleset is persisted only once confirmed, so a boot
// never brings back a change nobody proved reachable.
const (
	persistPath = "/etc/krill/firewall.nft"
	bootUnit    = "krill-firewall.service"
	bootUnitDir = "/etc/systemd/system"
)

// probeScript prints one "key value..." line per fact. addr is the node's swarm
// address (an IP literal validated by the caller), or "" to skip the lookup.
func probeScript(addr, file string) string {
	return fmt.Sprintf(`# krill-fw-probe
addr=%[1]s
if command -v nft >/dev/null 2>&1; then echo "nft yes"; else echo "nft no"; fi
if nft list table inet krill >/dev/null 2>&1; then
	echo "table yes"
	nft list table inet krill 2>/dev/null | sed -n 's/^[[:space:]]*chain \(%[2]s[0-9a-f]*\).*/live \1/p'
else
	echo "table no"
fi
if systemctl is-active --quiet %[3]s.timer 2>/dev/null; then echo "pending yes"; else echo "pending no"; fi
if [ -f %[4]s ]; then
	echo "persisted yes"
	sed -n 's/^[[:space:]]*chain \(%[2]s[0-9a-f]*\).*/persisted-id \1/p' %[4]s
fi
if systemctl is-enabled --quiet %[5]s 2>/dev/null; then echo "unit yes"; else echo "unit no"; fi
if [ -n "$addr" ]; then
	ip -o addr show 2>/dev/null | awk -v a="$addr" '{split($4,p,"/"); if (p[1]==a) print "iface", $2, $4}'
fi
ip -o link show 2>/dev/null | awk '{n=$2; sub(":$","",n); sub("@.*$","",n); for(i=1;i<NF;i++) if($i=="mtu") print "mtu", n, $(i+1)}'
ip -o link show type wireguard 2>/dev/null | awk '{n=$2; sub(":$","",n); print "wg", n}'
if command -v wg >/dev/null 2>&1; then
	echo "wgtool yes"
	# wireguard-go and boringtun interfaces are tun devices that
	# "ip link show type wireguard" does not list; wg knows them.
	for i in $(wg show interfaces 2>/dev/null); do echo "wg $i"; done
	wg show all listen-port 2>/dev/null | awk '{print "wgport", $1, $2}'
fi
`, shellQuote(addr), markerPrefix, revertUnit, file, bootUnit)
}

// Probe reads a node's lockdown and network state. swarmAddr may be nil.
func Probe(ctx context.Context, r Runner, swarmAddr net.IP) (Host, error) {
	addr := ""
	if swarmAddr != nil {
		addr = swarmAddr.String()
	}
	out, err := r.Run(ctx, "", probeScript(addr, persistPath))
	if err != nil {
		return Host{}, fmt.Errorf("probe: %w: %s", err, strings.TrimSpace(out))
	}
	return parseProbe(out), nil
}

func parseProbe(out string) Host {
	var h Host
	mtus := map[string]int{}
	wg := map[string]bool{}
	var iface *Iface
	wgTool := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "nft":
			h.Nft = f[1] == "yes"
		case "table":
			h.Table = f[1] == "yes"
		case "live":
			h.LiveID = f[1]
		case "pending":
			h.Pending = f[1] == "yes"
		case "persisted":
			h.Persisted = f[1] == "yes"
		case "persisted-id":
			h.PersistedID = f[1]
		case "unit":
			h.UnitEnabled = f[1] == "yes"
		case "iface":
			if len(f) >= 3 && iface == nil {
				if _, n, err := net.ParseCIDR(f[2]); err == nil {
					iface = &Iface{Name: f[1], Net: n}
				}
			}
		case "mtu":
			if len(f) >= 3 {
				if v, err := strconv.Atoi(f[2]); err == nil {
					mtus[f[1]] = v
				}
			}
		case "wg":
			if !wg[f[1]] {
				wg[f[1]] = true
				h.WireGuard = append(h.WireGuard, f[1])
			}
		case "wgtool":
			wgTool = f[1] == "yes"
		case "wgport":
			if len(f) >= 3 {
				if v, err := strconv.Atoi(f[2]); err == nil && v > 0 && v <= 65535 {
					h.WireGuardPorts = append(h.WireGuardPorts, v)
				}
			}
		}
	}
	if iface != nil {
		iface.MTU = mtus[iface.Name]
		iface.WireGuard = wg[iface.Name]
		h.SwarmIface = iface
	}
	h.WireGuardPortsKnown = len(h.WireGuard) == 0 || wgTool
	return h
}

// Rules completes base with what this node needs: its WireGuard listen ports,
// and — when the node's swarm address sits on a WireGuard interface whose
// subnet holds every swarm address in the cluster — that interface as the only
// one swarm traffic is accepted on. A mixed cluster (some node reached over
// another interface) keeps the address-only rules, which do not depend on the
// interface a peer's packets arrive on.
func (h Host) Rules(base Rules, swarmAddrs []net.IP) (Rules, error) {
	if !h.WireGuardPortsKnown {
		return Rules{}, ErrWireGuardPortsUnknown
	}
	r := base
	r.WireGuardUDP = append(append([]int(nil), base.WireGuardUDP...), h.WireGuardPorts...)
	if h.SwarmIface != nil && h.SwarmIface.WireGuard && len(swarmAddrs) > 0 {
		all := true
		for _, a := range swarmAddrs {
			if !h.SwarmIface.Net.Contains(a) {
				all = false
				break
			}
		}
		if all {
			r.ClusterIface = h.SwarmIface.Name
		}
	}
	return r, nil
}

// shellQuote wraps s in single quotes for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
