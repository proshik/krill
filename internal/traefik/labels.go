package traefik

import (
	"fmt"
	"regexp"
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
	Primary        bool     // the application's primary domain (redirect target)
	// RedirectToPrimary answers every request on the host with a permanent
	// redirect to the primary domain. It is routed whatever Exposed says, and
	// Paths, BasicAuthUsers, AllowedIPs and hidden metrics paths do not apply.
	RedirectToPrimary bool
}

// Redirect middleware parameters. Traefik matches the regex against the full
// request URL (scheme://host[:port]/path?query); the capture keeps the request
// URI. With permanent=true Traefik v3 answers 301 to GET and 308 to any other
// method, so a non-GET request keeps its method.
const (
	redirectRegex     = `^https?://[^/]+(.*)$`
	redirectURIToken  = "${1}"
	redirectPermanent = "true"
)

// primaryTarget is the scheme://host every redirecting domain points at, or ""
// when the list has no primary (or the primary is itself marked as redirecting,
// which the schema forbids).
func primaryTarget(domains []Domain) string {
	for _, d := range domains {
		if d.Primary && !d.RedirectToPrimary {
			if d.TLS {
				return "https://" + d.Host
			}
			return "http://" + d.Host
		}
	}
	return ""
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

// NormalizePaths makes legacy trailing slashes and a root prefix consistent
// with segment routing. Empty means catch-all; input paths are pre-validated.
func NormalizePaths(paths []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = strings.TrimRight(p, "/")
		if p == "" {
			return nil
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// Paths outrank a catch-all even when metrics protection makes its rule longer.
const (
	PriorityPaths    = 200
	PriorityCatchAll = 100
)

// domainRule builds the Traefik router rule for a host with an optional
// path allow-list (segment match) and an optional deny-list of metrics paths.
// Both lists must be pre-validated (no backticks). A hidden path is matched
// case-insensitively, together with everything under it and any ";" path
// parameter: backends that ignore case (ASP.NET) or strip ";..." (servlet
// containers) would otherwise serve /METRICS or /metrics;x from the domain.
func domainRule(host string, paths, hidden []string) string {
	paths = NormalizePaths(paths)
	rule := fmt.Sprintf("Host(`%s`)", host)
	if len(paths) > 0 {
		parts := make([]string, 0, len(paths))
		for _, p := range paths {
			parts = append(parts, fmt.Sprintf("(Path(`%s`) || PathPrefix(`%s/`))", p, p))
		}
		rule += " && (" + strings.Join(parts, " || ") + ")"
	}
	if len(hidden) > 0 {
		parts := make([]string, 0, len(hidden))
		for _, p := range hidden {
			parts = append(parts, fmt.Sprintf("PathRegexp(`(?i)^%s(?:[/;].*)?$`)", regexp.QuoteMeta(p)))
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
//
// A RedirectToPrimary domain gets Host-only routers carrying a redirectregex
// middleware to the primary domain (TLS: a websecure router with its own
// certificate plus a web router redirecting straight to the target). Without a
// primary in the list it is not routed at all. Traefik's ACME HTTP-01 router
// has the highest possible priority, so certificate challenges still work.
func AppLabels(serviceName string, domains []Domain, port int32, network string, hiddenPaths []string) map[string]string {
	target := primaryTarget(domains)
	redirects := func(d Domain) bool { return d.RedirectToPrimary && !d.Primary && target != "" }
	// A redirecting domain never serves the app, even with no target to go to.
	serves := func(d Domain) bool { return d.Exposed && !(d.RedirectToPrimary && !d.Primary) }
	routed := 0
	for _, d := range domains {
		if serves(d) || redirects(d) {
			routed++
		}
	}
	if routed == 0 {
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
		if redirects(d) {
			redirectLabels(l, serviceName, i, d, target)
			continue
		}
		if !serves(d) {
			continue
		}
		priority := strconv.Itoa(PriorityCatchAll)
		if len(NormalizePaths(d.Paths)) > 0 {
			priority = strconv.Itoa(PriorityPaths)
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
			l[rp+"priority"] = priority
			l[rp+"entrypoints"] = "web"
			l[rp+"service"] = serviceName
			l[rp+"middlewares"] = mw
			l["traefik.http.middlewares."+mw+".redirectscheme.scheme"] = "https"
		}
		l[servePrefix+"priority"] = priority
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

// redirectLabels emits the routers of one domain redirecting to target. The
// host is exclusive to this domain (internal/hostshare), so a Host-only rule at
// catch-all priority cannot shadow another application's paths.
func redirectLabels(l map[string]string, serviceName string, i int, d Domain, target string) {
	base := fmt.Sprintf("%s-d%d", serviceName, i)
	mw := base + "-toprimary"
	mp := "traefik.http.middlewares." + mw + ".redirectregex."
	l[mp+"regex"] = redirectRegex
	l[mp+"replacement"] = target + redirectURIToken
	l[mp+"permanent"] = redirectPermanent
	rule := fmt.Sprintf("Host(`%s`)", d.Host)
	priority := strconv.Itoa(PriorityCatchAll)
	router := func(name, entrypoint string) string {
		p := "traefik.http.routers." + name + "."
		l[p+"rule"] = rule
		l[p+"priority"] = priority
		l[p+"entrypoints"] = entrypoint
		l[p+"service"] = serviceName
		l[p+"middlewares"] = mw
		return p
	}
	// Plain HTTP redirects straight to the target in one hop, TLS or not.
	router(base, "web")
	if d.TLS {
		p := router(base+"s", "websecure")
		l[p+"tls.certresolver"] = "le"
	}
}
