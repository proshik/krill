package docker

import (
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
)

func TestPrefixNetworks(t *testing.T) {
	nets := []network.Summary{
		{ID: "n1", Name: "krill-egress-1", Scope: "swarm"},
		{ID: "n2", Name: "krill-egress-2", Scope: "swarm"},
		{ID: "n3", Name: "x-krill-egress-3", Scope: "swarm"}, // substring match only
		{ID: "n4", Name: "krill-egress-4", Scope: "local"},
		{ID: "org", Name: "krill-org-1", Scope: "swarm"},
	}
	att := func(ids ...string) []swarm.NetworkAttachmentConfig {
		var out []swarm.NetworkAttachmentConfig
		for _, id := range ids {
			out = append(out, swarm.NetworkAttachmentConfig{Target: id})
		}
		return out
	}
	svcs := []swarm.Service{
		{ID: "s-app", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "krill-1"}, TaskTemplate: swarm.TaskSpec{Networks: att("org", "n1")}}},
		{ID: "s-side", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "krill-link-egress"}, TaskTemplate: swarm.TaskSpec{Networks: att("n1", "krill-egress-2")}}},
	}
	task := func(svc string, state swarm.TaskState, netID, name, cidr string) swarm.Task {
		return swarm.Task{ServiceID: svc, NodeID: "node", Status: swarm.TaskStatus{State: state},
			NetworksAttachments: []swarm.NetworkAttachment{{Network: swarm.Network{ID: netID, Spec: swarm.NetworkSpec{Annotations: swarm.Annotations{Name: name}}}, Addresses: []string{cidr}}}}
	}
	tasks := []swarm.Task{
		task("s-app", swarm.TaskStateRunning, "n1", "krill-egress-1", "10.0.9.5/24"),
		task("s-app", swarm.TaskStateStarting, "n1", "krill-egress-1", "10.0.9.6/24"),
		task("s-app", swarm.TaskStateFailed, "n1", "krill-egress-1", "10.0.9.7/24"),
		task("s-app", swarm.TaskStateRunning, "org", "krill-org-1", "10.0.1.5/24"),
		task("s-side", swarm.TaskStateRunning, "n1", "krill-egress-1", "10.0.9.2/24"),
		task("s-gone", swarm.TaskStateRunning, "n1", "krill-egress-1", "10.0.9.9/24"),
	}
	got := prefixNetworks("krill-egress-", nets, svcs, tasks)
	want := []PrefixNetwork{
		{Name: "krill-egress-1", Services: []string{"krill-1", "krill-link-egress"}, Tasks: []NetworkTask{
			{Service: "krill-1", NodeID: "node", State: "running", IP: "10.0.9.5"},
			{Service: "krill-1", NodeID: "node", State: "starting", IP: "10.0.9.6"},
			{Service: "krill-link-egress", NodeID: "node", State: "running", IP: "10.0.9.2"},
		}},
		{Name: "krill-egress-2", Services: []string{"krill-link-egress"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
