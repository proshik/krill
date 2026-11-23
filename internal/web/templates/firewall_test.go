package templates

import "testing"

// A worker without nft is left as it is by a lockdown, which goes on with the
// other nodes; only a control plane without nft makes it unavailable.
func TestFirewallLockdownAvailability(t *testing.T) {
	cases := []struct {
		name  string
		nodes []FirewallNode
		want  bool
	}{
		{"worker without nft", []FirewallNode{{Name: "control-plane", ControlPlane: true, State: "open"}, {Name: "w1", State: "nft_missing"}}, false},
		{"control plane without nft", []FirewallNode{{Name: "control-plane", ControlPlane: true, State: "nft_missing"}}, true},
		{"all have nft", []FirewallNode{{Name: "control-plane", ControlPlane: true, State: "locked"}}, false},
	}
	for _, c := range cases {
		if got := fwControlPlaneNoNft(c.nodes); got != c.want {
			t.Errorf("%s: lockdown disabled = %v, want %v", c.name, got, c.want)
		}
	}
}
