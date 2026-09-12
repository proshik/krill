package docker

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
)

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
	// Env is deterministic (sorted).
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
	// Phase 0/2 regression: empty Type => bind
	spec := ServiceSpec{
		Name:   "krill-traefik",
		Image:  "traefik:v3.6.1",
		Mounts: []MountSpec{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}},
	}
	sw := buildSwarmSpec(spec)
	if string(sw.TaskTemplate.ContainerSpec.Mounts[0].Type) != "bind" {
		t.Errorf("empty Type must map to bind, got %q", sw.TaskTemplate.ContainerSpec.Mounts[0].Type)
	}
	// without DNSRR — VIP
	if spec.Ports == nil && sw.EndpointSpec != nil && string(sw.EndpointSpec.Mode) == "dnsrr" {
		t.Error("non-DNSRR spec should not be dnsrr")
	}
}

func TestBuildSwarmSpecAdvanced(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{
		Name: "krill-1", Image: "nginx:latest", Network: "krill-net",
		MemoryLimitBytes: 268435456, NanoCPUs: 500000000,
		RestartCondition: "on-failure", RestartMaxAttempts: 3,
		Healthcheck: &HealthcheckSpec{
			Test:     []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"},
			Interval: 30 * time.Second, Timeout: 5 * time.Second,
			StartPeriod: 10 * time.Second, Retries: 3,
		},
	})
	res := sw.TaskTemplate.Resources
	if res == nil || res.Limits == nil || res.Limits.MemoryBytes != 268435456 || res.Limits.NanoCPUs != 500000000 {
		t.Fatalf("resources not set correctly: %+v", res)
	}
	rp := sw.TaskTemplate.RestartPolicy
	if rp == nil || rp.Condition != swarm.RestartPolicyConditionOnFailure || rp.MaxAttempts == nil || *rp.MaxAttempts != 3 {
		t.Fatalf("restart policy wrong: %+v", rp)
	}
	hc := sw.TaskTemplate.ContainerSpec.Healthcheck
	if hc == nil || len(hc.Test) != 2 || hc.Interval != 30*time.Second || hc.Retries != 3 {
		t.Fatalf("healthcheck wrong: %+v", hc)
	}
}

func TestBuildSwarmSpecAdvancedDefaults(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{Name: "krill-1", Image: "nginx", Network: "krill-net"})
	if sw.TaskTemplate.Resources != nil {
		t.Errorf("expected no resources, got %+v", sw.TaskTemplate.Resources)
	}
	rp := sw.TaskTemplate.RestartPolicy
	if rp == nil || rp.Condition != swarm.RestartPolicyConditionAny || rp.MaxAttempts != nil {
		t.Errorf("expected restart any/unlimited, got %+v", rp)
	}
	if sw.TaskTemplate.ContainerSpec.Healthcheck != nil {
		t.Errorf("expected no healthcheck, got %+v", sw.TaskTemplate.ContainerSpec.Healthcheck)
	}
}

func TestRestartCondition(t *testing.T) {
	cases := []struct {
		in   string
		want swarm.RestartPolicyCondition
	}{
		{"on-failure", swarm.RestartPolicyConditionOnFailure},
		{"none", swarm.RestartPolicyConditionNone},
		{"any", swarm.RestartPolicyConditionAny},
		{"", swarm.RestartPolicyConditionAny},      // empty defaults to "any"
		{"bogus", swarm.RestartPolicyConditionAny}, // unknown defaults to "any"
	}
	for _, tc := range cases {
		if got := restartCondition(tc.in); got != tc.want {
			t.Errorf("restartCondition(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildSwarmSpecRestartConditionNone verifies the "none" condition reaches
// the Swarm spec and, with RestartMaxAttempts == 0 (the boundary meaning
// "unlimited"), no MaxAttempts pointer is set.
func TestBuildSwarmSpecRestartConditionNone(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{
		Name: "krill-1", Image: "nginx", Network: "krill-net",
		RestartCondition: "none", RestartMaxAttempts: 0,
	})
	rp := sw.TaskTemplate.RestartPolicy
	if rp == nil {
		t.Fatal("restart policy is nil")
	}
	if rp.Condition != swarm.RestartPolicyConditionNone {
		t.Errorf("condition = %q, want %q", rp.Condition, swarm.RestartPolicyConditionNone)
	}
	if rp.MaxAttempts != nil {
		t.Errorf("RestartMaxAttempts == 0 must leave MaxAttempts nil (unlimited), got %d", *rp.MaxAttempts)
	}
}

// A service that has to live in several networks (the Traefik gateway, which
// must reach every organization's otherwise isolated network) sets Networks;
// every entry has to become a task attachment.
func TestSpecAttachesEveryNetwork(t *testing.T) {
	s := buildSwarmSpec(ServiceSpec{Name: "x", Image: "i", Networks: []string{"a", "b"}})
	if len(s.TaskTemplate.Networks) != 2 {
		t.Fatalf("want 2 networks, got %+v", s.TaskTemplate.Networks)
	}
	if s.TaskTemplate.Networks[0].Target != "a" || s.TaskTemplate.Networks[1].Target != "b" {
		t.Fatalf("attachment order must follow Networks, got %+v", s.TaskTemplate.Networks)
	}
}

// Networks is optional: a service with a single network keeps using Network.
func TestSpecFallsBackToSingleNetwork(t *testing.T) {
	s := buildSwarmSpec(ServiceSpec{Name: "x", Image: "i", Network: "krill-net"})
	if len(s.TaskTemplate.Networks) != 1 || s.TaskTemplate.Networks[0].Target != "krill-net" {
		t.Fatalf("want the single Network attached, got %+v", s.TaskTemplate.Networks)
	}
}
