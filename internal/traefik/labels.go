package traefik

import (
	"fmt"
	"strconv"
)

// AppLabels builds the service-level Traefik labels for the application.
// serviceName is the Swarm service name (also the router/service id in Traefik).
func AppLabels(serviceName, domain string, port int32, network string) map[string]string {
	rp := "traefik.http.routers." + serviceName + "."
	sp := "traefik.http.services." + serviceName + ".loadbalancer.server.port"
	return map[string]string{
		"traefik.enable":         "true",
		rp + "rule":              fmt.Sprintf("Host(`%s`)", domain),
		rp + "entrypoints":       "web",
		rp + "service":           serviceName,
		sp:                       strconv.Itoa(int(port)),
		"traefik.docker.network": network,
	}
}
