package traefik

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/proshik/krill/internal/docker"
)

// TraefikVersion is the Traefik image version.
// v3.6.1+ is required: in it the swarm provider auto-negotiates the Docker API
// (traefik#12253/#12256). Versions ≤3.6.0 hardcode API 1.24 and Engine 29.x
// rejects them ("client version 1.24 is too old"), so routers are not built.
const TraefikVersion = "v3.6.1"

// ServiceName is the fixed Swarm service name of the Traefik ingress proxy.
const ServiceName = "krill-traefik"

// specHashLabel carries a fingerprint of the spec Krill last applied to the
// gateway. ServiceDeploy bumps ForceUpdate unconditionally, so this label is
// the only way to tell "nothing changed" from "redeploy me" without recreating
// the task on every call.
const specHashLabel = "krill.traefik.spec-hash"

// AcmeConfig holds the Let's Encrypt settings for the Traefik resolver.
type AcmeConfig struct {
	Email   string
	Staging bool
}

// TraefikSpec builds the spec for the Traefik Swarm service.
// Configured via CLI arguments; only the docker socket is mounted (no traefik.yml).
//
// networks is the full set the gateway attaches to, the base network first.
// That first entry is also the swarm provider's default network — the one a
// router carrying no explicit network label is resolved on.
func TraefikSpec(networks []string, acme AcmeConfig) docker.ServiceSpec {
	defaultNet := ""
	if len(networks) > 0 {
		defaultNet = networks[0]
	}
	args := []string{
		"--providers.swarm.endpoint=unix:///var/run/docker.sock",
		"--providers.swarm.exposedByDefault=false",
		"--providers.swarm.network=" + defaultNet,
		"--entrypoints.web.address=:80",
		"--entrypoints.websecure.address=:443",
		"--certificatesresolvers.le.acme.email=" + acme.Email,
		"--certificatesresolvers.le.acme.storage=/letsencrypt/acme.json",
		"--certificatesresolvers.le.acme.httpchallenge=true",
		"--certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
	}
	if acme.Staging {
		args = append(args, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory")
	}
	spec := docker.ServiceSpec{
		Name:     ServiceName,
		Image:    "traefik:" + TraefikVersion,
		Replicas: 1,
		Network:  defaultNet,
		Networks: networks,
		Args:     args,
		Ports: []docker.PortSpec{
			{Target: 80, Published: 80, Mode: "host"},
			{Target: 443, Published: 443, Mode: "host"},
		},
		Mounts: []docker.MountSpec{
			{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true},
			{Type: "volume", Source: "krill-traefik-acme", Target: "/letsencrypt"},
		},
		Constraints: []string{"node.role==manager"},
	}
	spec.Labels = map[string]string{specHashLabel: specFingerprint(spec)}
	return spec
}

// specFingerprint hashes everything the spec says about the service, so a
// changed image, argument, network or mount all show up. An empty result means
// "unknown"; it never matches a stored label, so the caller falls back to
// deploying.
func specFingerprint(s docker.ServiceSpec) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Reconcile makes sure Traefik is attached to the base network and to every
// organization network. With one overlay network per organization the gateway
// is the one service that has to live in all of them: those networks are
// isolated from each other, and the ingress path crosses that boundary by
// design.
//
// Swarm recreates the Traefik task whenever the network set changes, so ingress
// blips for a moment — the accepted price of one gateway serving networks that
// are otherwise isolated. It is also why an unchanged spec is not redeployed:
// ServiceDeploy bumps ForceUpdate unconditionally, so calling it on every
// startup and every organization change would blip ingress for nothing.
func Reconcile(ctx context.Context, eng docker.Engine, baseNetwork string, orgNetworks []string, acme AcmeConfig) error {
	nets := make([]string, 0, len(orgNetworks)+1)
	nets = append(nets, baseNetwork)
	nets = append(nets, orgNetworks...)
	// A network that had to be created is a network whose id is new. Swarm
	// stores network IDs in a service spec, not names, so the gateway's stored
	// attachment still points at the old, deleted network even though the name
	// set — and therefore the fingerprint — is unchanged. Redeploy regardless.
	created := false
	for _, n := range nets {
		c, err := eng.NetworkEnsure(ctx, n)
		if err != nil {
			return err
		}
		created = created || c
	}
	spec := TraefikSpec(nets, acme)
	// A failed or empty read is not a reason to skip: it only means we cannot
	// tell whether anything changed, and a redundant restart beats a gateway
	// that never learns about a new network.
	if !created {
		if cur, found, err := eng.ServiceLabels(ctx, ServiceName); err == nil && found {
			if h := spec.Labels[specHashLabel]; h != "" && cur[specHashLabel] == h {
				return nil
			}
		}
	}
	return eng.ServiceDeploy(ctx, spec)
}
