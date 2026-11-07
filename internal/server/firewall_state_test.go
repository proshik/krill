package server

import (
	"errors"
	"net"
	"reflect"
	"testing"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/web/templates"
)

func TestClassifyFirewall(t *testing.T) {
	const cur, old = "krill_rs_0000000000000001", "krill_rs_0000000000000002"
	persistedCur := firewall.Host{Nft: true, Table: true, LiveID: cur, Persisted: true, PersistedID: cur, UnitEnabled: true}
	cases := []struct {
		name    string
		h       firewall.Host
		exp     string
		managed bool
		state   string
		drift   []string
	}{
		{"open", firewall.Host{Nft: true}, cur, false, "open", nil},
		{"no nft", firewall.Host{}, cur, true, "nft_missing", nil},
		{"current", persistedCur, cur, true, "locked", nil},
		{"outdated", persistedCur, old, true, "locked", []string{"firewall.drift.outdated"}},
		{"expected unknown", persistedCur, "", true, "locked", nil},
		{"foreign", firewall.Host{Nft: true, Table: true, Persisted: true, UnitEnabled: true}, cur, false, "locked", []string{"firewall.drift.foreign"}},
		{"not persisted", firewall.Host{Nft: true, Table: true, LiveID: cur}, cur, true, "locked", []string{"firewall.drift.not_persisted"}},
		{"unit disabled", firewall.Host{Nft: true, Table: true, LiveID: cur, Persisted: true, PersistedID: cur}, cur, true, "locked", []string{"firewall.drift.not_persisted"}},
		{"persisted copy differs", firewall.Host{Nft: true, Table: true, LiveID: cur, Persisted: true, PersistedID: old, UnitEnabled: true}, cur, true, "locked", []string{"firewall.drift.not_persisted"}},
		// An unconfirmed change is not expected to be persisted yet.
		{"pending", firewall.Host{Nft: true, Table: true, LiveID: cur, Pending: true}, cur, false, "pending", nil},
		{"missing managed", firewall.Host{Nft: true}, cur, true, "missing", []string{"firewall.drift.missing"}},
		{"missing persisted", firewall.Host{Nft: true, Persisted: true}, cur, false, "missing", []string{"firewall.drift.missing"}},
	}
	for _, c := range cases {
		state, drift := classifyFirewall(c.h, c.exp, c.managed)
		if state != c.state || !reflect.DeepEqual(drift, c.drift) {
			t.Errorf("%s: got %s %v, want %s %v", c.name, state, drift, c.state, c.drift)
		}
	}
}

func TestOverlayMTUView(t *testing.T) {
	nodes := []templates.FirewallNode{
		{Name: "control-plane", Iface: "wg0", MTU: 1420},
		{Name: "worker-app", Iface: "wg0", MTU: 1420},
		{Name: "worker-db", State: "unavailable"},
	}
	mtus := map[string]int{"krill-net": 1450, "krill-org-1": 1450, "krill-org-2": 1370, "ingress": 1450, "other": 1450}
	v := overlayMTUView(0, "krill-net", nodes, mtus)
	if v.Effective != 1450 || v.MinIface != 1420 || v.Recommended != 1370 || !v.TooLarge {
		t.Fatalf("view: %+v", v)
	}
	if len(v.Networks) != 3 || v.Networks[0].Name != "krill-net" || !v.Networks[0].TooLarge || v.Networks[2].TooLarge {
		t.Fatalf("networks: %+v", v.Networks)
	}
	// Configured to the recommendation: nothing too large, the old networks
	// differ from what a new one gets.
	v = overlayMTUView(1370, "krill-net", nodes, mtus)
	if v.TooLarge || !v.Networks[1].Differs || v.Networks[2].Differs {
		t.Fatalf("configured: %+v", v)
	}
	// A plain 1500 underlay: Docker's default fits.
	v = overlayMTUView(0, "krill-net", []templates.FirewallNode{{Name: "cp", Iface: "eth0", MTU: 1500}}, nil)
	if v.TooLarge || v.Available || v.Recommended != 1450 {
		t.Fatalf("plain: %+v", v)
	}
}

// Binding swarm ports to the WireGuard interface needs every node's address:
// a failed node listing or a node without one yields no list, so no binding.
func TestSwarmAddrSetOnlyWhenComplete(t *testing.T) {
	cp := net.ParseIP("10.8.0.1")
	nodes := []docker.SwarmNode{{ID: "m", Role: "manager", Addr: "0.0.0.0"}, {ID: "w1", Role: "worker", Addr: "10.8.0.2"}}
	byNode, all := swarmAddrSet(cp, nodes, nil)
	if len(all) != 2 || byNode["w1"].String() != "10.8.0.2" {
		t.Fatalf("complete: %v %v", all, byNode)
	}
	if _, all := swarmAddrSet(cp, nodes, errors.New("daemon busy")); all != nil {
		t.Fatalf("failed listing must give no list: %v", all)
	}
	if _, all := swarmAddrSet(cp, append(nodes, docker.SwarmNode{ID: "w2", Role: "worker", Addr: ""}), nil); all != nil {
		t.Fatalf("a worker without an address must give no list: %v", all)
	}
	if _, all := swarmAddrSet(nil, nodes, nil); all != nil {
		t.Fatalf("an unknown control plane must give no list: %v", all)
	}
}
