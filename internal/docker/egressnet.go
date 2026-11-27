package docker

import (
	"context"
	"net"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
)

// PrefixNetwork is one swarm network whose name starts with a prefix, with
// what uses it.
type PrefixNetwork struct {
	Name string
	// Services are the services whose current spec attaches the network.
	Services []string
	// Tasks are the tasks attached to it that Swarm wants running, with
	// their address on it: running, and starting or preparing ones too (a
	// task waiting for its healthcheck already makes connections).
	Tasks []NetworkTask
}

// NetworkTask is one task's address on a network.
type NetworkTask struct {
	Service string
	NodeID  string
	State   string
	IP      string // without the prefix length
}

// PrefixNetworks is the optional capability to list the networks whose name
// starts with a prefix and what uses them (the Krill link's egress networks).
// Only *dockerEngine implements it.
type PrefixNetworks interface {
	PrefixNetworks(ctx context.Context, prefix string) ([]PrefixNetwork, error)
}

var _ PrefixNetworks = (*dockerEngine)(nil)

func (e *dockerEngine) PrefixNetworks(ctx context.Context, prefix string) ([]PrefixNetwork, error) {
	nets, err := e.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", prefix))})
	if err != nil {
		return nil, err
	}
	svcs, err := e.cli.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("desired-state", "running"))})
	if err != nil {
		return nil, err
	}
	return prefixNetworks(prefix, nets, svcs, tasks), nil
}

// prefixNetworks builds PrefixNetworks' answer from what the daemon listed.
func prefixNetworks(prefix string, nets []network.Summary, svcs []swarm.Service, tasks []swarm.Task) []PrefixNetwork {
	byID := map[string]*PrefixNetwork{}
	byName := map[string]*PrefixNetwork{}
	var order []*PrefixNetwork
	for _, n := range nets {
		// The name filter matches substrings; the scope keeps local
		// networks of the same name out.
		if !strings.HasPrefix(n.Name, prefix) || n.Scope != "swarm" {
			continue
		}
		p := &PrefixNetwork{Name: n.Name}
		byID[n.ID], byName[n.Name] = p, p
		order = append(order, p)
	}
	svcName := make(map[string]string, len(svcs))
	for _, s := range svcs {
		svcName[s.ID] = s.Spec.Name
		for _, a := range s.Spec.TaskTemplate.Networks {
			// Swarm stores the network ID even when the spec named it.
			p := byID[a.Target]
			if p == nil {
				p = byName[a.Target]
			}
			if p != nil {
				p.Services = append(p.Services, s.Spec.Name)
			}
		}
	}
	for _, t := range tasks {
		switch t.Status.State {
		case swarm.TaskStateAssigned, swarm.TaskStateAccepted, swarm.TaskStatePreparing,
			swarm.TaskStateReady, swarm.TaskStateStarting, swarm.TaskStateRunning:
		default:
			continue
		}
		name, ok := svcName[t.ServiceID]
		if !ok {
			continue
		}
		for _, att := range t.NetworksAttachments {
			p := byID[att.Network.ID]
			if p == nil {
				p = byName[att.Network.Spec.Name]
			}
			if p == nil {
				continue
			}
			for _, cidr := range att.Addresses {
				ip, _, err := net.ParseCIDR(cidr)
				if err != nil {
					continue
				}
				p.Tasks = append(p.Tasks, NetworkTask{Service: name, NodeID: t.NodeID, State: string(t.Status.State), IP: ip.String()})
			}
		}
	}
	out := make([]PrefixNetwork, 0, len(order))
	for _, p := range order {
		sort.Strings(p.Services)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
