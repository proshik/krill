package docker

import (
	"sort"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

// buildSwarmSpec конвертирует ServiceSpec в swarm.ServiceSpec.
func buildSwarmSpec(s ServiceSpec) swarm.ServiceSpec {
	replicas := s.Replicas

	envs := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		envs = append(envs, k+"="+v)
	}
	sort.Strings(envs) // детерминизм для тестов и диффов

	cs := &swarm.ContainerSpec{Image: s.Image, Env: envs}
	if len(s.Command) > 0 {
		cs.Command = s.Command
	}
	if len(s.Args) > 0 {
		cs.Args = s.Args
	}
	for _, m := range s.Mounts {
		cs.Mounts = append(cs.Mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}

	task := swarm.TaskSpec{
		ContainerSpec: cs,
		Networks:      []swarm.NetworkAttachmentConfig{{Target: s.Network}},
		RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny},
	}
	if len(s.Constraints) > 0 {
		task.Placement = &swarm.Placement{Constraints: s.Constraints}
	}

	spec := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: s.Name, Labels: s.Labels},
		TaskTemplate: task,
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			Order:         swarm.UpdateOrderStartFirst,       // zero-downtime
			FailureAction: swarm.UpdateFailureActionRollback,
		},
		RollbackConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			FailureAction: swarm.UpdateFailureActionPause,
		},
	}

	if len(s.Ports) > 0 {
		ports := make([]swarm.PortConfig, 0, len(s.Ports))
		for _, p := range s.Ports {
			proto := swarm.PortConfigProtocolTCP
			if p.UDP {
				proto = swarm.PortConfigProtocolUDP
			}
			pm := swarm.PortConfigPublishModeIngress
			if p.Mode == "host" {
				pm = swarm.PortConfigPublishModeHost
			}
			ports = append(ports, swarm.PortConfig{
				Protocol:      proto,
				TargetPort:    p.Target,
				PublishedPort: p.Published,
				PublishMode:   pm,
			})
		}
		spec.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP, Ports: ports}
	}
	return spec
}
