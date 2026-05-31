package traefik

import "testing"

func TestAppLabels(t *testing.T) {
	l := AppLabels("krill-web", "web.127-0-0-1.sslip.io", 80, "krill-net")
	cases := map[string]string{
		"traefik.enable":                            "true",
		"traefik.http.routers.krill-web.rule":       "Host(`web.127-0-0-1.sslip.io`)",
		"traefik.http.routers.krill-web.entrypoints": "web",
		"traefik.http.routers.krill-web.service":    "krill-web",
		"traefik.http.services.krill-web.loadbalancer.server.port": "80",
		"traefik.docker.network":                    "krill-net",
	}
	for k, want := range cases {
		if l[k] != want {
			t.Errorf("label %q = %q, want %q", k, l[k], want)
		}
	}
}
