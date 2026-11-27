package link

import (
	"errors"
	"testing"
)

func TestRollbackGate(t *testing.T) {
	cases := []struct {
		name, kind, version string
		ok                  bool
		prev                string
	}{
		{"no krill.prev", RollbackAbsent, "", true, ""},
		{"older than the core", RollbackRelease, "v0.16.3", false, "v0.16.3"},
		{"the core release", RollbackRelease, CoreRelease, true, ""},
		{"newer", RollbackRelease, "v0.18.0", true, ""},
		{"a prerelease of the core", RollbackRelease, CoreRelease + "-rc.1", false, CoreRelease + "-rc.1"},
		{"unknown", "unknown", "", false, ""},
		{"a release kind without a version", RollbackRelease, "", false, ""},
		{"garbage version", RollbackRelease, "dev+abc", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RollbackGate(tc.kind, tc.version)
			if tc.ok {
				if err != nil {
					t.Fatalf("RollbackGate = %v, want nil", err)
				}
				return
			}
			var r *RollbackRefusal
			if !errors.As(err, &r) || r.Prev != tc.prev {
				t.Fatalf("RollbackGate = %v, want a refusal naming %q", err, tc.prev)
			}
		})
	}
}

// Every combination: only all four facts true let a connection through, and
// the reason names the first fact that fails.
func TestDecideEgress(t *testing.T) {
	for i := 0; i < 16; i++ {
		f := egressFacts{session: i&1 != 0, granted: i&2 != 0, selected: i&4 != 0, relay: i&8 != 0}
		err := decideEgress(f)
		var want error
		switch {
		case !f.session:
			want = ErrEgressNoSession
		case !f.granted:
			want = ErrEgressNotGranted
		case !f.selected:
			want = ErrEgressNotSelected
		case !f.relay:
			want = ErrEgressNotAllowed
		}
		if !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Errorf("%+v: decideEgress = %v, want %v", f, err, want)
		}
	}
}

func TestParseDestination(t *testing.T) {
	ok := []struct{ host, port, want string }{
		{"api.telegram.org", "443", "api.telegram.org:443"},
		{" API.OpenAI.com. ", " 8443 ", "api.openai.com:8443"},
		{"a.example.com", "1", "a.example.com:1"},
		{"a.example.com", "65535", "a.example.com:65535"},
	}
	for _, c := range ok {
		d, err := ParseDestination(c.host, c.port)
		if err != nil || d.String() != c.want {
			t.Errorf("ParseDestination(%q, %q) = (%v, %v), want %s", c.host, c.port, d, err, c.want)
		}
	}
	bad := []struct{ host, port string }{
		{"203.0.113.7", "443"}, {"*.example.com", "443"}, {"localhost", "443"}, {"", "443"},
		{"a.example.com", "0"}, {"a.example.com", "65536"}, {"a.example.com", "x"}, {"a.example.com", ""},
		{"a.example.com", "-1"}, {"[::1]", "443"},
	}
	for _, c := range bad {
		if _, err := ParseDestination(c.host, c.port); !errors.Is(err, ErrBadDestination) {
			t.Errorf("ParseDestination(%q, %q) err = %v, want ErrBadDestination", c.host, c.port, err)
		}
	}
	if d, err := ParseDestinationString("api.example.com:443"); err != nil || d != (Destination{"api.example.com", 443}) {
		t.Errorf("ParseDestinationString = %v, %v", d, err)
	}
}

func TestEgressNetworkApp(t *testing.T) {
	if EgressNetwork(42) != "krill-egress-42" {
		t.Fatal(EgressNetwork(42))
	}
	for name, want := range map[string]int64{"krill-egress-42": 42, "krill-egress-": 0, "krill-egress-042": 0, "krill-egress-x": 0, "krill-org-3": 0, "krill-egress--1": 0} {
		got, ok := EgressNetworkApp(name)
		if got != want || ok != (want != 0) {
			t.Errorf("EgressNetworkApp(%q) = %d, %v", name, got, ok)
		}
	}
}
