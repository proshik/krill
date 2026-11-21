package traefik

import (
	"strings"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestTraefikSpecTLS(t *testing.T) {
	s := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c", Staging: false}, PanelProvider{})
	if !hasArg(s.Args, "--entrypoints.websecure.address=:443") {
		t.Error("missing websecure entrypoint")
	}
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.email=a@b.c") {
		t.Error("missing acme email")
	}
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.httpchallenge.entrypoint=web") {
		t.Error("missing http challenge entrypoint")
	}
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.storage=/letsencrypt/acme.json") {
		t.Error("missing acme storage")
	}
	var has443, hasVol bool
	for _, p := range s.Ports {
		if p.Target == 443 && p.Published == 443 {
			has443 = true
		}
	}
	for _, m := range s.Mounts {
		if m.Type == "volume" && m.Target == "/letsencrypt" {
			hasVol = true
		}
	}
	if !has443 || !hasVol {
		t.Errorf("443 port=%v acme volume=%v", has443, hasVol)
	}
	for _, a := range s.Args {
		if strings.Contains(a, "caserver") {
			t.Error("prod must not set a staging caserver")
		}
	}
}

func TestTraefikSpecStaging(t *testing.T) {
	s := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c", Staging: true}, PanelProvider{})
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory") {
		t.Error("staging caserver missing")
	}
}

func TestTraefikSpecPanelProvider(t *testing.T) {
	plain := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, PanelProvider{})
	for _, a := range plain.Args {
		if strings.HasPrefix(a, "--providers.http.") {
			t.Fatalf("no endpoint must leave the HTTP provider out, got %q", a)
		}
	}
	p := PanelProvider{Endpoint: "http://198.51.100.10:8080/_krill/gateway/config", Header: "X-Krill-Gateway-Provider", Token: "tok"}
	s := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, p)
	for _, want := range []string{
		"--providers.http.endpoint=http://198.51.100.10:8080/_krill/gateway/config",
		"--providers.http.pollInterval=5s",
		"--providers.http.headers.X-Krill-Gateway-Provider=tok",
		"--providers.swarm.endpoint=unix:///var/run/docker.sock",
	} {
		if !hasArg(s.Args, want) {
			t.Errorf("missing %q", want)
		}
	}
	if s.Labels[specHashLabel] == plain.Labels[specHashLabel] {
		t.Error("enabling the provider must change the fingerprint so the gateway is redeployed once")
	}
	if again := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, p); again.Labels[specHashLabel] != s.Labels[specHashLabel] {
		t.Error("the same provider must fingerprint identically, or every start would redeploy the gateway")
	}
}

func hasPrefixArg(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// The gateway's timeouts are spelled out, so a Traefik upgrade cannot change
// what a long response or an upload is allowed to do.
func TestTraefikSpecSetsTimeoutsExplicitly(t *testing.T) {
	s := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, PanelProvider{})
	for _, ep := range []string{"web", "websecure"} {
		for _, want := range []string{
			"--entrypoints." + ep + ".transport.respondingTimeouts.readTimeout=60s",
			"--entrypoints." + ep + ".transport.respondingTimeouts.writeTimeout=0s",
			"--entrypoints." + ep + ".transport.respondingTimeouts.idleTimeout=180s",
		} {
			if !hasArg(s.Args, want) {
				t.Errorf("missing %s", want)
			}
		}
	}
	if !hasArg(s.Args, "--serversTransport.forwardingTimeouts.responseHeaderTimeout=0s") {
		t.Error("missing responseHeaderTimeout")
	}
}

func TestTraefikSpecAccessLogModes(t *testing.T) {
	t.Cleanup(func() { SetAccessLog(AccessLogOff) })
	spec := func(m AccessLog) docker.ServiceSpec {
		SetAccessLog(m)
		return TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, PanelProvider{})
	}
	off := spec(AccessLogOff)
	if hasPrefixArg(off.Args, "--accesslog") {
		t.Fatalf("off renders access log args: %v", off.Args)
	}
	all := spec(AccessLogAll)
	if !hasArg(all.Args, "--accesslog=true") || !hasArg(all.Args, "--accesslog.fields.headers.defaultmode=drop") || hasPrefixArg(all.Args, "--accesslog.filters") {
		t.Fatalf("all: %v", all.Args)
	}
	filtered := spec(AccessLogFiltered)
	for _, want := range []string{"--accesslog=true", "--accesslog.filters.statuscodes=400-599", "--accesslog.filters.minduration=10s", "--accesslog.filters.retryattempts=true"} {
		if !hasArg(filtered.Args, want) {
			t.Errorf("filtered: missing %s", want)
		}
	}
	// Changing the mode must redeploy the gateway: the fingerprint differs.
	if off.Labels[specHashLabel] == filtered.Labels[specHashLabel] {
		t.Fatal("access log mode does not change the spec fingerprint")
	}
}

func TestParseAccessLog(t *testing.T) {
	for in, want := range map[string]AccessLog{"": AccessLogOff, "off": AccessLogOff, "Filtered": AccessLogFiltered, " all ": AccessLogAll} {
		got, err := ParseAccessLog(in)
		if err != nil || got != want {
			t.Errorf("ParseAccessLog(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseAccessLog("errors"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
