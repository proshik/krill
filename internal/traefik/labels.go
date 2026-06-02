package traefik

import (
	"fmt"
	"strconv"
)

// Domain is one host routed to an application, with optional TLS.
type Domain struct {
	Host string
	TLS  bool
}

// AppLabels builds the service-level Traefik labels for an application's domains.
// serviceName is the Swarm service name (and the Traefik loadbalancer-service id).
// Non-TLS domains get a plain router on the web entrypoint. TLS domains get a
// secure router on websecure (cert via the "le" resolver) plus a web router that
// redirects to HTTPS. There is no global redirect, so plain-HTTP domains keep working.
func AppLabels(serviceName string, domains []Domain, port int32, network string) map[string]string {
	svc := "traefik.http.services." + serviceName + ".loadbalancer.server.port"
	l := map[string]string{
		"traefik.enable":         "true",
		"traefik.docker.network": network,
		svc:                      strconv.Itoa(int(port)),
	}
	for i, d := range domains {
		base := fmt.Sprintf("%s-d%d", serviceName, i)
		rule := fmt.Sprintf("Host(`%s`)", d.Host)
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
