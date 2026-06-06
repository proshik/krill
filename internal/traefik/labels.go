package traefik

import (
	"fmt"
	"strconv"
	"strings"
)

// Domain is one host routed to an application, with optional TLS.
type Domain struct {
	Host    string
	TLS     bool
	Exposed bool
	Paths   []string // allowed path prefixes; empty = all
}

// SplitPaths parses a stored newline-separated paths string into trimmed,
// non-empty prefixes.
func SplitPaths(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// domainRule builds the Traefik router rule for a host with an optional
// path allow-list (prefix match). Paths must be pre-validated (no backticks).
func domainRule(host string, paths []string) string {
	rule := fmt.Sprintf("Host(`%s`)", host)
	if len(paths) == 0 {
		return rule
	}
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		parts = append(parts, fmt.Sprintf("PathPrefix(`%s`)", p))
	}
	return rule + " && (" + strings.Join(parts, " || ") + ")"
}

// AppLabels builds the service-level Traefik labels for an application's domains.
// serviceName is the Swarm service name (and the Traefik loadbalancer-service id).
// Non-TLS domains get a plain router on the web entrypoint. TLS domains get a
// secure router on websecure (cert via the "le" resolver) plus a web router that
// redirects to HTTPS. There is no global redirect, so plain-HTTP domains keep working.
func AppLabels(serviceName string, domains []Domain, port int32, network string) map[string]string {
	exposed := 0
	for _, d := range domains {
		if d.Exposed {
			exposed++
		}
	}
	if exposed == 0 {
		// internal-only: Traefik ignores the service entirely.
		return map[string]string{"traefik.enable": "false"}
	}
	svc := "traefik.http.services." + serviceName + ".loadbalancer.server.port"
	l := map[string]string{
		"traefik.enable":         "true",
		"traefik.docker.network": network,
		svc:                      strconv.Itoa(int(port)),
	}
	for i, d := range domains {
		if !d.Exposed {
			continue
		}
		base := fmt.Sprintf("%s-d%d", serviceName, i)
		rule := domainRule(d.Host, d.Paths)
		if !d.TLS {
			rp := "traefik.http.routers." + base + "."
			l[rp+"rule"] = rule
			l[rp+"entrypoints"] = "web"
			l[rp+"service"] = serviceName
			continue
		}
		sp := "traefik.http.routers." + base + "s."
		l[sp+"rule"] = rule
		l[sp+"entrypoints"] = "websecure"
		l[sp+"service"] = serviceName
		l[sp+"tls.certresolver"] = "le"
		mw := base + "-redirect"
		rp := "traefik.http.routers." + base + "."
		l[rp+"rule"] = rule
		l[rp+"entrypoints"] = "web"
		l[rp+"service"] = serviceName
		l[rp+"middlewares"] = mw
		l["traefik.http.middlewares."+mw+".redirectscheme.scheme"] = "https"
	}
	return l
}
