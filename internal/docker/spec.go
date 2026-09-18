package docker

import (
	"os"
	"sort"

	container "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

// buildSwarmSpec converts a ServiceSpec into a swarm.ServiceSpec.
func buildSwarmSpec(s ServiceSpec) swarm.ServiceSpec {
	replicas := s.Replicas

	envs := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		envs = append(envs, k+"="+v)
	}
	sort.Strings(envs) // determinism for tests and diffs

	cs := &swarm.ContainerSpec{Image: s.Image, Env: envs}
	if len(s.Command) > 0 {
		cs.Command = s.Command
	}
	if len(s.Args) > 0 {
		cs.Args = s.Args
	}
	for _, m := range s.Mounts {
		mt := mount.TypeBind
		if m.Type == "volume" {
			mt = mount.TypeVolume
		}
		cs.Mounts = append(cs.Mounts, mount.Mount{
			Type:     mt,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}

	cs.Hostname = s.Hostname
	if len(s.ContainerLabels) > 0 {
		cs.Labels = s.ContainerLabels
	}
	for _, c := range s.Configs {
		cs.Configs = append(cs.Configs, &swarm.ConfigReference{
			File:       &swarm.ConfigReferenceFileTarget{Name: c.Target, UID: "0", GID: "0", Mode: fileMode(c.Mode)},
			ConfigID:   c.ID,
			ConfigName: c.Name,
		})
	}
	for _, sc := range s.Secrets {
		cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
			File:       &swarm.SecretReferenceFileTarget{Name: sc.Target, UID: "0", GID: "0", Mode: fileMode(sc.Mode)},
			SecretID:   sc.ID,
			SecretName: sc.Name,
		})
	}

	if s.Healthcheck != nil {
		cs.Healthcheck = &container.HealthConfig{
			Test:        s.Healthcheck.Test,
			Interval:    s.Healthcheck.Interval,
			Timeout:     s.Healthcheck.Timeout,
			StartPeriod: s.Healthcheck.StartPeriod,
			Retries:     s.Healthcheck.Retries,
		}
	}

	rp := &swarm.RestartPolicy{Condition: restartCondition(s.RestartCondition)}
	if s.RestartMaxAttempts > 0 {
		ma := s.RestartMaxAttempts
		rp.MaxAttempts = &ma
	}

	// Networks wins when set; Network is the single-network shorthand every
	// service but the gateway uses.
	nets := s.Networks
	if len(nets) == 0 {
		nets = []string{s.Network}
	}
	attachments := make([]swarm.NetworkAttachmentConfig, 0, len(nets))
	for _, n := range nets {
		attachments = append(attachments, swarm.NetworkAttachmentConfig{Target: n})
	}

	task := swarm.TaskSpec{
		ContainerSpec: cs,
		Networks:      attachments,
		RestartPolicy: rp,
	}
	if s.MemoryLimitBytes > 0 || s.NanoCPUs > 0 {
		task.Resources = &swarm.ResourceRequirements{
			Limits: &swarm.Limit{MemoryBytes: s.MemoryLimitBytes, NanoCPUs: s.NanoCPUs},
		}
	}
	if len(s.Constraints) > 0 || s.SpreadNodeID {
		p := &swarm.Placement{}
		if len(s.Constraints) > 0 {
			p.Constraints = s.Constraints
		}
		if s.SpreadNodeID {
			p.Preferences = []swarm.PlacementPreference{{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.id"}}}
		}
		task.Placement = p
	}

	svcMode := swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}
	if s.Global {
		svcMode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
	}
	spec := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: s.Name, Labels: s.Labels},
		TaskTemplate: task,
		Mode:         svcMode,
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			Order:         updateOrder(s),
			FailureAction: swarm.UpdateFailureActionRollback,
		},
		RollbackConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			FailureAction: swarm.UpdateFailureActionPause,
		},
	}

	mode := swarm.ResolutionModeVIP
	if s.DNSRR {
		mode = swarm.ResolutionModeDNSRR
	}
	if s.DNSRR || len(s.Ports) > 0 {
		ep := &swarm.EndpointSpec{Mode: mode}
		for _, p := range s.Ports {
			proto := swarm.PortConfigProtocolTCP
			if p.UDP {
				proto = swarm.PortConfigProtocolUDP
			}
			pm := swarm.PortConfigPublishModeIngress
			if p.Mode == "host" {
				pm = swarm.PortConfigPublishModeHost
			}
			ep.Ports = append(ep.Ports, swarm.PortConfig{
				Protocol: proto, TargetPort: p.Target, PublishedPort: p.Published, PublishMode: pm,
			})
		}
		spec.EndpointSpec = ep
	}
	return spec
}

// updateOrder is start-first (zero downtime) unless the service publishes a
// host-mode port or asks for UpdateStopFirst explicitly. A host-mode port is
// bound by the task on its node, so a start-first replacement cannot be placed
// while the task it replaces still holds the port: it stays Pending with
// "host-mode port conflict", and because start-first stops the old task only
// once the new one runs, the update never finishes and never fails — the old
// spec just keeps running. Traefik (:80/:443 on the manager), raw TCP/UDP app
// ports and database proxies all publish in host mode, so they stop the old
// task first and accept a brief gap. UpdateStopFirst covers the same situation
// for a service that holds something else node-local instead of a port, like
// an agent's WAL directory.
func updateOrder(s ServiceSpec) string {
	if s.UpdateStopFirst {
		return swarm.UpdateOrderStopFirst
	}
	for _, p := range s.Ports {
		if p.Mode == "host" {
			return swarm.UpdateOrderStopFirst
		}
	}
	return swarm.UpdateOrderStartFirst
}

// fileMode defaults an unset mode to world-readable, Swarm's own default.
func fileMode(m os.FileMode) os.FileMode {
	if m == 0 {
		return 0o444
	}
	return m
}

// restartCondition maps a human restart condition to the Swarm enum.
func restartCondition(c string) swarm.RestartPolicyCondition {
	switch c {
	case "on-failure":
		return swarm.RestartPolicyConditionOnFailure
	case "none":
		return swarm.RestartPolicyConditionNone
	default:
		return swarm.RestartPolicyConditionAny
	}
}
