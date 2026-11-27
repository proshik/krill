package server

import (
	"context"
	"testing"
)

type nftOut string

func (o nftOut) Run(context.Context, string, string) (string, error) { return string(o), nil }

// The UI port counts as closed to the world only when the gateway-only rule
// lists it — not when that rule exists for the link listener alone.
func TestPortGatewayOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want bool
	}{
		"ui closed, nft single":   {"table inet krill {\n\tchain input {\n\t\ttcp dport { 80, 443 } accept\n\t\tiifname \"docker_gwbridge\" tcp dport 8080 accept\n\t}\n}\n", true},
		"ui closed with link":     {"\t\tiifname \"docker_gwbridge\" tcp dport { 8080, 8445 } accept\n", true},
		"ui open, link only":      {"\t\ttcp dport { 80, 443, 8080 } accept\n\t\tiifname \"docker_gwbridge\" tcp dport 8445 accept\n", false},
		"ui open, built ruleset":  {"    tcp dport { 80, 443, 8080 } accept\n    iifname \"docker_gwbridge\" tcp dport { 8445 } accept\n", false},
		"no table":                {"", false},
		"one line":                {`table inet krill { chain input { iifname "docker_gwbridge" tcp dport 8080 accept } }`, true},
		"one line, link only":     {`table inet krill { chain input { iifname "docker_gwbridge" tcp dport 8445 accept } }`, false},
		"port is a prefix of one": {"\t\tiifname \"docker_gwbridge\" tcp dport 80800 accept\n", false},
	} {
		got, err := portGatewayOnly(context.Background(), nftOut(tc.out), 8080)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v %v, want %v", name, got, err, tc.want)
		}
	}
}
