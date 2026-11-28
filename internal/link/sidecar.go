package link

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// SidecarService is the Swarm service of the egress sidecar: the pinned
// Traefik on a manager, on every app's egress network, answering for that
// app's destination hosts and handing each TLS connection, unopened, to the
// link listener.
const SidecarService = "krill-link-egress"

// sidecarHashLabel carries the fingerprint of the sidecar spec Krill last
// applied: ServiceDeploy recreates the task on every call, so an unchanged
// spec is not deployed again.
const sidecarHashLabel = "krill.link.egress.spec-hash"

// sidecarObjectLabel marks the sidecar's Swarm secrets, so old ones can be
// pruned.
const (
	sidecarObjectLabel = "krill.link"
	sidecarObjectValue = "egress-config"
)

// sidecarReplicas is how many tasks the sidecar runs.
const sidecarReplicas = 2

// sidecarConfigFile is the static configuration's file name under
// /run/secrets.
const sidecarConfigFile = "krill-link-egress.yaml"

// EgressConfigPath is where the sidecar's HTTP provider polls its dynamic
// configuration, and EgressHeader the header that carries its token.
const (
	EgressConfigPath = "/_krill/link/egress-config"
	EgressHeader     = "X-Krill-Link-Egress-Token"
)

// EgressToken derives the sidecar's provider token from the gateway's stored
// secret (panel.EnsureSecret), under its own label: it never equals the
// panel's tokens. An empty secret gives no token, which nothing matches.
func EgressToken(gatewaySecret string) string {
	if gatewaySecret == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(gatewaySecret))
	m.Write([]byte("krill-gateway/link-egress"))
	return hex.EncodeToString(m.Sum(nil))
}

// sidecarInput is what the sidecar's spec is made of.
type sidecarInput struct {
	// Aliases are, per egress network, the hosts the sidecar answers for
	// there; a network with none is still attached (an app leaving it).
	Aliases     map[string][]string
	Ports       []int  // one entrypoint each
	Name        string // the service; "" for SidecarService
	ProviderURL string // the dynamic configuration's URL
	Token       string // the provider token
}

// entrypoint names the entrypoint of a port.
func entrypoint(port int) string { return "p" + strconv.Itoa(port) }

// sidecarSpec builds the sidecar's service spec and its static configuration,
// which goes into a Swarm secret named in the spec: the configuration holds
// the provider token, and a container's arguments are visible to anyone who
// can inspect it. The secret's name is derived from its content, so a new
// configuration is a new secret and a spec change.
func sidecarSpec(in sidecarInput) (docker.ServiceSpec, []byte) {
	ports := slices.Sorted(slices.Values(in.Ports))
	ports = slices.Compact(ports)
	eps := make(map[string]any, len(ports))
	for _, p := range ports {
		eps[entrypoint(p)] = map[string]any{"address": ":" + strconv.Itoa(p)}
	}
	cfg := map[string]any{
		"providers": map[string]any{
			"http": map[string]any{
				"endpoint":     in.ProviderURL,
				"pollInterval": "5s",
				"headers":      map[string]string{EgressHeader: in.Token},
			},
		},
		"log": map[string]any{"level": "WARN"},
	}
	// An empty entryPoints object stops Traefik ("entryPoints cannot be a
	// standalone element"): with no port left (an app leaving its network)
	// the key is left out, and Traefik's default web entrypoint routes
	// nothing.
	if len(eps) > 0 {
		cfg["entryPoints"] = eps
	}
	static, _ := json.Marshal(cfg)
	sum := sha256.Sum256(static)
	name := in.Name
	if name == "" {
		name = SidecarService
	}
	secretName := name + "-" + hex.EncodeToString(sum[:])[:16]

	nets := slices.Sorted(maps.Keys(in.Aliases))
	aliases := map[string][]string{}
	for _, n := range nets {
		hs := slices.Sorted(slices.Values(in.Aliases[n]))
		if hs = slices.Compact(hs); len(hs) > 0 {
			aliases[n] = hs
		}
	}
	spec := docker.ServiceSpec{
		Name:  name,
		Image: "traefik:" + traefik.TraefikVersion,
		// Two tasks: with none, Docker's DNS stops answering for the
		// aliases and falls back to the real addresses (fail open; checked
		// on Docker 29.5.2 for DNSRR and VIP alike), so one task failing
		// must not leave the names unanswered while Swarm restarts it.
		Replicas: sidecarReplicas,
		Networks: nets,
		// DNSRR: an alias resolves to the sidecar task's own address, so the
		// connection reaches it — and Krill — from the app's task address,
		// which a VIP would replace with the network's balancer.
		DNSRR:          true,
		NetworkAliases: aliases,
		Args:           []string{"--configFile=/run/secrets/" + sidecarConfigFile},
		Secrets:        []docker.FileRef{{Name: secretName, Target: sidecarConfigFile, Mode: 0o400}},
		// On the managers, as the gateway: the sidecar reaches Krill's
		// listener on the advertise address through docker_gwbridge.
		Constraints: []string{"node.role==manager"},
		LogDriver:   &docker.LogDriverSpec{Name: "json-file", Options: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	if len(nets) > 0 {
		spec.Network = nets[0]
	}
	spec.Labels = map[string]string{sidecarHashLabel: specHash(spec)}
	return spec, static
}

// specHash fingerprints a spec ("" when it cannot be serialized, which never
// matches a stored label).
func specHash(s docker.ServiceSpec) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Names of the sidecar's objects in its dynamic configuration.
const (
	egressService   = "krill-link-egress"
	egressTransport = "krill-link-sidecar"
)

// EgressDynamicConfig is the sidecar's dynamic configuration: per port, a TCP
// router matching the allowed hosts by SNI with passthrough, to the link
// listener (upstream) through a transport that sends PROXY v2 and then opens
// TLS with the gateway's client certificate. A host not routed reaches no
// backend. No routes give an empty configuration.
func EgressDynamicConfig(routes []Destination, upstream string, local LocalMaterial) map[string]any {
	byPort := map[int][]string{}
	for _, d := range SortDestinations(routes) {
		byPort[d.Port] = append(byPort[d.Port], d.Host)
	}
	if len(byPort) == 0 {
		return map[string]any{}
	}
	routers := map[string]any{}
	for port, hosts := range byPort {
		rules := make([]string, 0, len(hosts))
		for _, h := range hosts {
			rules = append(rules, fmt.Sprintf("HostSNI(`%s`)", h))
		}
		routers["egress-"+entrypoint(port)] = map[string]any{
			"rule":        strings.Join(rules, " || "),
			"entryPoints": []string{entrypoint(port)},
			"service":     egressService,
			"tls":         map[string]any{"passthrough": true},
		}
	}
	return map[string]any{"tcp": map[string]any{
		"routers": routers,
		"services": map[string]any{
			egressService: map[string]any{
				"loadBalancer": map[string]any{
					"servers":          []map[string]any{{"address": upstream, "tls": true}},
					"serversTransport": egressTransport,
				},
			},
		},
		"serversTransports": map[string]any{
			egressTransport: map[string]any{
				"proxyProtocol": map[string]any{"version": 2},
				"tls": map[string]any{
					"serverName":   ListenerServerName,
					"rootCAs":      []string{local.Listener.CertPEM},
					"certificates": []map[string]any{{"certFile": local.Gateway.CertPEM, "keyFile": local.Gateway.KeyPEM}},
				},
			},
		},
	}}
}
