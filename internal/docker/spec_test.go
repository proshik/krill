package docker

import "testing"

func TestBuildSwarmSpec(t *testing.T) {
	spec := ServiceSpec{
		Name:     "krill-web",
		Image:    "nginx:alpine",
		Env:      map[string]string{"B": "2", "A": "1"},
		Labels:   map[string]string{"traefik.enable": "true"},
		Replicas: 1,
		Network:  "krill-net",
	}
	sw := buildSwarmSpec(spec)

	if sw.Annotations.Name != "krill-web" {
		t.Errorf("name = %q", sw.Annotations.Name)
	}
	if sw.Annotations.Labels["traefik.enable"] != "true" {
		t.Error("service labels not propagated")
	}
	if sw.TaskTemplate.ContainerSpec.Image != "nginx:alpine" {
		t.Error("image not set")
	}
	// Env детерминирован (отсортирован).
	got := sw.TaskTemplate.ContainerSpec.Env
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("env = %v, want [A=1 B=2]", got)
	}
	if len(sw.TaskTemplate.Networks) != 1 || sw.TaskTemplate.Networks[0].Target != "krill-net" {
		t.Error("network not attached at TaskTemplate level")
	}
	if sw.Mode.Replicated == nil || *sw.Mode.Replicated.Replicas != 1 {
		t.Error("replicas wrong")
	}
	if sw.UpdateConfig == nil || sw.UpdateConfig.Order != "start-first" {
		t.Error("update config should be start-first")
	}
}

func TestBuildSwarmSpecHostPortAndMounts(t *testing.T) {
	spec := ServiceSpec{
		Name:    "krill-traefik",
		Image:   "traefik:v3.5.0",
		Network: "krill-net",
		Ports:   []PortSpec{{Target: 80, Published: 80, Mode: "host"}},
		Mounts:  []MountSpec{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true}},
		Args:    []string{"--entrypoints.web.address=:80"},
	}
	sw := buildSwarmSpec(spec)
	if sw.EndpointSpec == nil || len(sw.EndpointSpec.Ports) != 1 {
		t.Fatal("endpoint ports missing")
	}
	if sw.EndpointSpec.Ports[0].PublishMode != "host" {
		t.Errorf("publish mode = %v", sw.EndpointSpec.Ports[0].PublishMode)
	}
	if len(sw.TaskTemplate.ContainerSpec.Mounts) != 1 {
		t.Fatal("mounts missing")
	}
	if !sw.TaskTemplate.ContainerSpec.Mounts[0].ReadOnly {
		t.Error("mount should be read-only")
	}
	if len(sw.TaskTemplate.ContainerSpec.Args) != 1 {
		t.Error("args missing")
	}
}

func TestBuildSwarmSpecVolumeAndDNSRR(t *testing.T) {
	spec := ServiceSpec{
		Name:    "krill-pg-x",
		Image:   "postgres:17",
		Network: "krill-net",
		DNSRR:   true,
		Mounts:  []MountSpec{{Type: "volume", Source: "krill-pg-x-data", Target: "/var/lib/postgresql/data"}},
	}
	sw := buildSwarmSpec(spec)
	if len(sw.TaskTemplate.ContainerSpec.Mounts) != 1 {
		t.Fatalf("mounts: %d", len(sw.TaskTemplate.ContainerSpec.Mounts))
	}
	m := sw.TaskTemplate.ContainerSpec.Mounts[0]
	if string(m.Type) != "volume" || m.Source != "krill-pg-x-data" || m.Target != "/var/lib/postgresql/data" {
		t.Errorf("volume mount wrong: %+v", m)
	}
	if sw.EndpointSpec == nil || string(sw.EndpointSpec.Mode) != "dnsrr" {
		t.Errorf("expected dnsrr endpoint mode, got %+v", sw.EndpointSpec)
	}
}

func TestBuildSwarmSpecBindStillDefault(t *testing.T) {
	// регрессия Phase 0/2: пустой Type => bind
	spec := ServiceSpec{
		Name:   "krill-traefik",
		Image:  "traefik:v3.6.1",
		Mounts: []MountSpec{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}},
	}
	sw := buildSwarmSpec(spec)
	if string(sw.TaskTemplate.ContainerSpec.Mounts[0].Type) != "bind" {
		t.Errorf("empty Type must map to bind, got %q", sw.TaskTemplate.ContainerSpec.Mounts[0].Type)
	}
	// без DNSRR — VIP
	if spec.Ports == nil && sw.EndpointSpec != nil && string(sw.EndpointSpec.Mode) == "dnsrr" {
		t.Error("non-DNSRR spec should not be dnsrr")
	}
}
