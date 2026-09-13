package traefik

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

// convergeTimeout bounds how long Reconcile waits for the gateway's new task to
// run, and convergePoll is how often it looks. Variables so tests can shorten
// them.
var (
	convergeTimeout = 3 * time.Minute
	convergePoll    = time.Second
)

// AcmeConfig holds the Let's Encrypt settings for the Traefik resolver.
type AcmeConfig struct {
	Email   string
	Staging bool
}

// PanelProvider points the gateway's HTTP provider at Krill, which serves the
// routes of its own UI there (see internal/panel). The zero value leaves the
// provider out: Krill has no address the gateway could reach it at.
type PanelProvider struct {
	Endpoint string // full URL of the dynamic-configuration endpoint
	Header   string // request header carrying Token
	Token    string
}

// TraefikSpec builds the spec for the Traefik Swarm service.
// Configured via CLI arguments; only the docker socket is mounted (no traefik.yml).
//
// networks is the full set the gateway attaches to, the base network first.
// That first entry is also the swarm provider's default network — the one a
// router carrying no explicit network label is resolved on.
//
// The panel provider's arguments are derived from configuration and a stored
// secret, never from the panel's own settings, so they are identical on every
// start and do not disturb the spec fingerprint; the routes themselves change
// through the provider's poll without touching the service.
func TraefikSpec(networks []string, acme AcmeConfig, panel PanelProvider) docker.ServiceSpec {
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
	if panel.Endpoint != "" {
		args = append(args,
			"--providers.http.endpoint="+panel.Endpoint,
			"--providers.http.pollInterval=5s",
			"--providers.http.headers."+panel.Header+"="+panel.Token,
		)
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
//
// A deploy only hands Swarm the new spec, so Reconcile returns once the new
// task is actually running, and fails when it does not start in time or Swarm
// rolls it back. Until then the old task keeps serving the old networks, and a
// caller that took the deploy for done would move services into networks the
// gateway never joined.
func Reconcile(ctx context.Context, eng docker.Engine, baseNetwork string, orgNetworks []string, acme AcmeConfig, panel PanelProvider) error {
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
	spec := TraefikSpec(nets, acme, panel)
	// The tasks running now are the baseline the new task is told apart from.
	// Without it the old task would pass for the new one, so a failed read is
	// an error rather than an unverified deploy.
	before, err := eng.ServiceProgress(ctx, ServiceName, nil)
	if err != nil {
		return fmt.Errorf("read gateway tasks: %w", err)
	}
	// A matching label proves the spec was written, not that its task ever
	// started: an update still in progress — one stuck behind a task holding
	// the host ports, say — carries the label of the spec it never reached.
	// A failed or empty label read is not a reason to skip either: it only
	// means we cannot tell whether anything changed, and a redundant restart
	// beats a gateway that never learns about a new network.
	if !created && updateSettled(before.UpdateState) {
		if cur, found, err := eng.ServiceLabels(ctx, ServiceName); err == nil && found {
			if h := spec.Labels[specHashLabel]; h != "" && cur[specHashLabel] == h {
				return nil
			}
		}
	}
	var baseline []string
	if before.Found {
		baseline = before.TaskIDs
	}
	slog.Info("deploying the gateway", "networks", len(nets))
	if err := eng.ServiceDeploy(ctx, spec); err != nil {
		return err
	}
	if err := waitGateway(ctx, eng, baseline); err != nil {
		return err
	}
	slog.Info("gateway deployed", "networks", len(nets))
	return nil
}

// updateSettled reports whether the gateway's last update has come to rest.
// "" means the service was never updated.
func updateSettled(state string) bool {
	switch state {
	case "updating", "paused", "rollback_started", "rollback_paused":
		return false
	}
	return true
}

// waitGateway polls until a gateway task outside baseline is running and Swarm
// has completed the update. A running task alone is not enough: for its monitor
// period Swarm still reports the update in progress and rolls it back if the
// task fails. The rollback check skips the first poll: Swarm keeps the previous
// update's status, and it may not have switched to this update yet.
func waitGateway(ctx context.Context, eng docker.Engine, baseline []string) error {
	cctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()
	var lastErr error
	for first := true; ; first = false {
		p, err := eng.ServiceProgress(cctx, ServiceName, baseline)
		switch {
		case err != nil:
			lastErr = err
		case p.Found && !first && strings.HasPrefix(p.UpdateState, "rollback"):
			return errors.New("gateway update rolled back: Swarm restored the previous Traefik spec")
		case p.Found && p.Desired > 0 && p.Running >= p.Desired && updateSettled(p.UpdateState):
			return nil
		}
		select {
		case <-cctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("gateway did not start within %s: %w", convergeTimeout, lastErr)
			}
			return fmt.Errorf("gateway did not start within %s: its new task is not running (see docker service ps %s)", convergeTimeout, ServiceName)
		case <-time.After(convergePoll):
		}
	}
}
