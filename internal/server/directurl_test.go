package server

import (
	"net"
	"testing"

	"github.com/proshik/krill/internal/config"
)

func addrs(cidrs ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, err
			}
			n.IP = ip
			out = append(out, n)
		}
		return out, nil
	}
}

// The advertise address is often the address swarm runs over — a WireGuard or
// VPC one — which nobody on the internet can reach; the page must show where
// the port is actually exposed.
func TestDirectURL(t *testing.T) {
	host := addrs("127.0.0.1/8", "10.0.0.1/24", "172.18.0.1/16", "100.101.2.3/10", "198.51.100.20/24", "2001:db8::1/64") // gitleaks:allow — CGNAT range
	cases := []struct {
		name, listen, advertise string
		addrs                   func() ([]net.Addr, error)
		want                    string
	}{
		{"private advertise, public on host", ":8080", "10.0.0.1", host, "http://198.51.100.20:8080"},
		{"public advertise", ":8080", "203.0.113.10", host, "http://203.0.113.10:8080"},
		{"advertise name", ":8080", "cp.example.com", host, "http://cp.example.com:8080"},
		{"bound to one address", "10.0.0.1:8080", "10.0.0.1", host, "http://10.0.0.1:8080"},
		{"behind NAT, nothing public", ":8080", "10.0.0.5", addrs("10.0.0.5/24", "100.64.0.9/10"), ""}, // gitleaks:allow — CGNAT range
		{"only IPv6 public", ":8080", "", addrs("10.0.0.5/24", "2a01:4f8::1/64"), "http://[2a01:4f8::1]:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: config.Config{ListenAddr: c.listen, AdvertiseAddr: c.advertise}, interfaceAddrs: c.addrs}
			if got := s.directURL(); got != c.want {
				t.Fatalf("directURL = %q, want %q", got, c.want)
			}
		})
	}
}
