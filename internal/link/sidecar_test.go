package link

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/traefik"
)

func testSidecarInput() sidecarInput {
	return sidecarInput{
		Aliases: map[string][]string{
			"krill-egress-7": {"api.telegram.org", "api.openai.com"},
			"krill-egress-9": {"api.openai.com"},
			"krill-egress-3": nil, // an app still leaving its network: no names
		},
		Ports:       []int{8443, 443, 443},
		ProviderURL: "http://203.0.113.10:8080" + EgressConfigPath,
		Token:       "tok-secret",
	}
}

// The sidecar is the pinned Traefik on a manager, DNSRR (the app's address
// reaches Krill unchanged), without the docker socket or the swarm provider,
// on every egress network with that app's names only, one entrypoint per
// distinct port, and its static configuration — the provider token in it — in
// a Swarm secret rather than in its arguments.
func TestSidecarSpec(t *testing.T) {
	in := testSidecarInput()
	sp, static := sidecarSpec(in)
	if sp.Name != SidecarService || sp.Image != "traefik:"+traefik.TraefikVersion {
		t.Fatalf("name %q image %q", sp.Name, sp.Image)
	}
	if !sp.DNSRR || !reflect.DeepEqual(sp.Constraints, []string{"node.role==manager"}) || sp.Replicas != 1 {
		t.Fatalf("dnsrr %v constraints %v replicas %d", sp.DNSRR, sp.Constraints, sp.Replicas)
	}
	if len(sp.Mounts) != 0 || len(sp.Ports) != 0 {
		t.Fatalf("mounts %v ports %v: no docker socket, no published port", sp.Mounts, sp.Ports)
	}
	if !reflect.DeepEqual(sp.Networks, []string{"krill-egress-3", "krill-egress-7", "krill-egress-9"}) {
		t.Fatalf("networks %v", sp.Networks)
	}
	wantAliases := map[string][]string{"krill-egress-7": {"api.openai.com", "api.telegram.org"}, "krill-egress-9": {"api.openai.com"}}
	if !reflect.DeepEqual(sp.NetworkAliases, wantAliases) {
		t.Fatalf("aliases %v", sp.NetworkAliases)
	}
	for _, a := range sp.Args {
		if strings.Contains(a, in.Token) {
			t.Fatalf("token in the arguments: %v", sp.Args)
		}
	}
	for k, v := range sp.Labels {
		if strings.Contains(v, in.Token) {
			t.Fatalf("token in label %s", k)
		}
	}
	if len(sp.Secrets) != 1 || !strings.HasPrefix(sp.Secrets[0].Name, SidecarService+"-") {
		t.Fatalf("secrets %+v", sp.Secrets)
	}
	if !reflect.DeepEqual(sp.Args, []string{"--configFile=/run/secrets/" + sp.Secrets[0].Target}) {
		t.Fatalf("args %v", sp.Args)
	}
	var cfg struct {
		EntryPoints map[string]struct{ Address string } `json:"entryPoints"`
		Providers   struct {
			Swarm *struct{} `json:"swarm"`
			HTTP  struct {
				Endpoint string            `json:"endpoint"`
				Headers  map[string]string `json:"headers"`
			} `json:"http"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(static, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.EntryPoints) != 2 || cfg.EntryPoints["p443"].Address != ":443" || cfg.EntryPoints["p8443"].Address != ":8443" {
		t.Fatalf("entrypoints %+v", cfg.EntryPoints)
	}
	if cfg.Providers.Swarm != nil || cfg.Providers.HTTP.Endpoint != in.ProviderURL || cfg.Providers.HTTP.Headers[EgressHeader] != in.Token {
		t.Fatalf("providers %+v", cfg.Providers)
	}
}

// Unchanged inputs give the same spec, fingerprint and secret name, so a
// reconcile with nothing new does not recreate the task; a changed alias does.
func TestSidecarSpecFingerprint(t *testing.T) {
	a, _ := sidecarSpec(testSidecarInput())
	in := testSidecarInput()
	in.Aliases["krill-egress-7"] = []string{"api.openai.com", "api.telegram.org"} // same set, another order
	b, _ := sidecarSpec(in)
	if a.Labels[sidecarHashLabel] == "" || a.Labels[sidecarHashLabel] != b.Labels[sidecarHashLabel] || a.Secrets[0].Name != b.Secrets[0].Name {
		t.Fatalf("equal inputs differ: %v / %v", a.Labels, b.Labels)
	}
	in.Aliases["krill-egress-9"] = nil
	c, _ := sidecarSpec(in)
	if c.Labels[sidecarHashLabel] == a.Labels[sidecarHashLabel] {
		t.Fatal("a removed alias kept the fingerprint")
	}
	in = testSidecarInput()
	in.Token = "another"
	d, _ := sidecarSpec(in)
	if d.Secrets[0].Name == a.Secrets[0].Name || d.Labels[sidecarHashLabel] == a.Labels[sidecarHashLabel] {
		t.Fatal("a new token kept the secret")
	}
}

// The sidecar's dynamic configuration routes each allowed host by SNI, with
// passthrough, to the link listener through a transport that sends PROXY v2
// and the gateway's client certificate. Nothing else is routed.
func TestEgressDynamicConfig(t *testing.T) {
	local := LocalMaterial{Gateway: KeyPair{CertPEM: "GW-CERT", KeyPEM: "GW-KEY"}, Listener: KeyPair{CertPEM: "LS-CERT"}}
	routes := []Destination{{"api.telegram.org", 443}, {"api.openai.com", 443}, {"api.openai.com", 8443}}
	cfg := EgressDynamicConfig(routes, "203.0.113.10:8445", local)
	b, _ := json.Marshal(cfg)
	var got struct {
		TCP struct {
			Routers map[string]struct {
				Rule        string   `json:"rule"`
				EntryPoints []string `json:"entryPoints"`
				Service     string   `json:"service"`
				TLS         struct {
					Passthrough bool `json:"passthrough"`
				} `json:"tls"`
			} `json:"routers"`
			Services map[string]struct {
				LoadBalancer struct {
					Servers []struct {
						Address string `json:"address"`
						TLS     bool   `json:"tls"`
					} `json:"servers"`
					ServersTransport string `json:"serversTransport"`
				} `json:"loadBalancer"`
			} `json:"services"`
			ServersTransports map[string]struct {
				ProxyProtocol struct{ Version int } `json:"proxyProtocol"`
				TLS           struct {
					ServerName   string   `json:"serverName"`
					RootCAs      []string `json:"rootCAs"`
					Certificates []struct {
						CertFile string `json:"certFile"`
						KeyFile  string `json:"keyFile"`
					} `json:"certificates"`
				} `json:"tls"`
			} `json:"serversTransports"`
		} `json:"tcp"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	r443, r8443 := got.TCP.Routers["egress-p443"], got.TCP.Routers["egress-p8443"]
	if r443.Rule != "HostSNI(`api.openai.com`) || HostSNI(`api.telegram.org`)" || !reflect.DeepEqual(r443.EntryPoints, []string{"p443"}) || !r443.TLS.Passthrough {
		t.Fatalf("p443 router %+v", r443)
	}
	if r8443.Rule != "HostSNI(`api.openai.com`)" || !r8443.TLS.Passthrough || len(got.TCP.Routers) != 2 {
		t.Fatalf("routers %+v", got.TCP.Routers)
	}
	svc := got.TCP.Services[r443.Service].LoadBalancer
	if len(svc.Servers) != 1 || svc.Servers[0].Address != "203.0.113.10:8445" || !svc.Servers[0].TLS {
		t.Fatalf("service %+v", svc)
	}
	tr := got.TCP.ServersTransports[svc.ServersTransport]
	if tr.ProxyProtocol.Version != 2 || tr.TLS.ServerName != ListenerServerName || tr.TLS.RootCAs[0] != "LS-CERT" ||
		tr.TLS.Certificates[0].CertFile != "GW-CERT" || tr.TLS.Certificates[0].KeyFile != "GW-KEY" {
		t.Fatalf("transport %+v", tr)
	}
	if c := EgressDynamicConfig(nil, "203.0.113.10:8445", local); len(c) != 0 {
		t.Fatalf("no routes: %v, want an empty configuration", c)
	}
}

func TestEgressTokenDistinct(t *testing.T) {
	a, b := EgressToken("s1"), EgressToken("s2")
	if a == "" || a == b || len(a) != 64 || a == EgressToken("") {
		t.Fatalf("tokens %q %q", a, b)
	}
}
