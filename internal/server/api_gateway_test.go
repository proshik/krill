package server

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/config"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// gatewayEngine places the gateway's one task on node m2 of three managers.
type gatewayEngine struct{ docker.Engine }

func (gatewayEngine) ServiceTasks(_ context.Context, name string) ([]docker.TaskPlacement, error) {
	if name != traefik.ServiceName {
		return nil, nil
	}
	return []docker.TaskPlacement{{NodeID: "m2", State: "running"}, {NodeID: "m3", State: "shutdown"}}, nil
}

func (gatewayEngine) Nodes(context.Context) ([]docker.SwarmNode, error) {
	return []docker.SwarmNode{
		{ID: "m1", Role: "manager", Addr: "192.0.2.1"},
		{ID: "m2", Role: "manager", Addr: "192.0.2.2"},
		{ID: "m3", Role: "manager", Addr: "192.0.2.3"},
	}, nil
}

// The gateway answers where its task runs (host-mode ports), not at every
// manager, plus at the configured advertise address.
func TestAPIGatewayAddressesFollowTheGatewayTask(t *testing.T) {
	s := &Server{cfg: config.Config{AdvertiseAddr: "198.51.100.9"}, engine: gatewayEngine{}}
	got, err := apiGateway{s: s}.Addresses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "198.51.100.9" || got[1] != "192.0.2.2" {
		t.Fatalf("addresses %v", got)
	}
}
