package docker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
)

// NetworkAliases puts the aliases of each network on its attachment, sorted;
// a network without aliases gets none.
func TestBuildSwarmSpecNetworkAliases(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{
		Name: "krill-link-egress", Image: "traefik:v3.6.1",
		Networks:       []string{"krill-egress-1", "krill-egress-2"},
		NetworkAliases: map[string][]string{"krill-egress-1": {"b.example.com", "a.example.com"}},
		DNSRR:          true,
	})
	want := []swarm.NetworkAttachmentConfig{
		{Target: "krill-egress-1", Aliases: []string{"a.example.com", "b.example.com"}},
		{Target: "krill-egress-2"},
	}
	if !reflect.DeepEqual(sw.TaskTemplate.Networks, want) {
		t.Fatalf("attachments = %+v, want %+v", sw.TaskTemplate.Networks, want)
	}
	if sw.EndpointSpec == nil || sw.EndpointSpec.Mode != swarm.ResolutionModeDNSRR {
		t.Fatalf("endpoint = %+v, want dnsrr", sw.EndpointSpec)
	}
}

// A spec without aliases serializes as before: the gateway and the agents
// fingerprint the serialized spec.
func TestNetworkAliasesOmitEmpty(t *testing.T) {
	b, err := json.Marshal(ServiceSpec{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "NetworkAliases") {
		t.Fatalf("unset NetworkAliases serialized: %s", b)
	}
}
