package docker

import (
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

	task := swarm.TaskSpec{
		ContainerSpec: cs,
		Networks:      []swarm.NetworkAttachmentConfig{{Target: s.Network}},
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
			Order:         swarm.UpdateOrderStartFirst, // zero-downtime
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
