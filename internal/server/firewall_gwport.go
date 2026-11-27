package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/firewall"
)

// portGatewayOnly reports whether the control plane's live table admits port
// only from the gateway interface — for the UI port, that it is closed to the
// world. It looks for the port itself: the gateway-only rule also carries the
// link listener's port while the link role is on, so "some port is
// gateway-only" no longer means the UI port is. A node with no table reports
// false.
func portGatewayOnly(ctx context.Context, r firewall.Runner, port int) (bool, error) {
	out, err := r.Run(ctx, "", "nft list table inet krill 2>/dev/null || true")
	if err != nil {
		return false, err
	}
	iface := fmt.Sprintf("iifname %q", firewall.GatewayInterface)
	for rest := out; ; {
		i := strings.Index(rest, iface)
		if i < 0 {
			break
		}
		rest = rest[i+len(iface):]
		_, after, ok := strings.Cut(rest, "tcp dport")
		if !ok {
			break
		}
		for _, p := range dportList(after) {
			if p == port {
				return true, nil
			}
		}
	}
	return false, nil
}

// dportList reads the port set after "tcp dport": "{ 8080, 8445 }" or a single
// port, as nft prints it back.
func dportList(s string) []int {
	s = strings.TrimSpace(s)
	var list string
	if strings.HasPrefix(s, "{") {
		end := strings.Index(s, "}")
		if end < 0 {
			return nil
		}
		list = s[1:end]
	} else {
		f := strings.Fields(s)
		if len(f) == 0 {
			return nil
		}
		list = f[0]
	}
	var out []int
	for _, f := range strings.Split(list, ",") {
		if p, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out = append(out, p)
		}
	}
	return out
}
