package traefik

import (
	"fmt"
	"strconv"
	"strings"
)

// Domain is one host routed to an application, with optional TLS.
type Domain struct {
	Host           string
	TLS            bool
	Exposed        bool
	Paths          []string // allowed path prefixes; empty = all
	BasicAuthUsers []string // htpasswd entries "user:bcrypthash"; empty = no basic-auth
	AllowedIPs     []string // CIDR/IP entries; empty = no IP filter
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
// path allow-list (prefix match) and an optional deny-list of exact metrics
// paths (the path itself and everything under it). Both lists must be
// pre-validated (no backticks).
func domainRule(host string, paths, hidden []string) string {
	rule := fmt.Sprintf("Host(`%s`)", host)
	if len(paths) > 0 {
		parts := make([]string, 0, len(paths))
		for _, p := range paths {
			parts = append(parts, fmt.Sprintf("PathPrefix(`%s`)", p))
		}
		rule += " && (" + strings.Join(parts, " || ") + ")"
	}
	if len(hidden) > 0 {
		parts := make([]string, 0, 2*len(hidden))
		for _, p := range hidden {
			parts = append(parts, fmt.Sprintf("Path(`%s`)", p), fmt.Sprintf("PathPrefix(`%s/`)", p))
		}
		rule += " && !(" + strings.Join(parts, " || ") + ")"
	}
	return rule
}

// hiddenPaths excludes metrics paths on the app HTTP port from every domain.
// AppLabels builds the service-level Traefik labels for an application's domains.
// serviceName is the Swarm service name (and the Traefik loadbalancer-service id).
// Non-TLS domains get a plain router on the web entrypoint. TLS domains get a
// secure router on websecure (cert via the "le" resolver) plus a web router that
// redirects to HTTPS. There is no global redirect, so plain-HTTP domains keep working.
func AppLabels(serviceName string, domains []Domain, port int32, network string, hiddenPaths []string) map[string]string {
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
		rule := domainRule(d.Host, d.Paths, hiddenPaths)
		var servePrefix string // router prefix that actually serves traffic
		if !d.TLS {
			servePrefix = "traefik.http.routers." + base + "."
			l[servePrefix+"rule"] = rule
			l[servePrefix+"entrypoints"] = "web"
			l[servePrefix+"service"] = serviceName
		} else {
			servePrefix = "traefik.http.routers." + base + "s."
			l[servePrefix+"rule"] = rule
			l[servePrefix+"entrypoints"] = "websecure"
			l[servePrefix+"service"] = serviceName
			l[servePrefix+"tls.certresolver"] = "le"
			mw := base + "-redirect"
			rp := "traefik.http.routers." + base + "."
			l[rp+"rule"] = rule
			l[rp+"entrypoints"] = "web"
			l[rp+"service"] = serviceName
			l[rp+"middlewares"] = mw
			l["traefik.http.middlewares."+mw+".redirectscheme.scheme"] = "https"
		}
		// Access protection on the serving router: IP filter first, then auth (AND).
		var mws []string
		if len(d.AllowedIPs) > 0 {
			name := base + "-ipallow"
			l["traefik.http.middlewares."+name+".ipallowlist.sourcerange"] = strings.Join(d.AllowedIPs, ",")
			mws = append(mws, name)
		}
		if len(d.BasicAuthUsers) > 0 {
			name := base + "-auth"
			l["traefik.http.middlewares."+name+".basicauth.users"] = strings.Join(d.BasicAuthUsers, ",")
			mws = append(mws, name)
		}
		if len(mws) > 0 {
			l[servePrefix+"middlewares"] = strings.Join(mws, ",")
		}
	}
	return l
}
