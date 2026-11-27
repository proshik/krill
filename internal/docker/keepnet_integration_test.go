//go:build integration

package docker_test

import (
	"context"
	"slices"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/proshik/krill/internal/docker"
)

// A redeploy with KeepNetworkPrefix keeps an attachment the running service
// has to a matching network, with its aliases, although the new spec does not
// list it; a redeploy without it drops the attachment.
func TestServiceDeployKeepsPrefixedAttachments(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	skipIfNoSwarm(t, e)
	ctx := context.Background()
	const (
		svc    = "krl001-it-keepnet"
		orgNet = "krl001-it-org"
		egress = "krill-egress-krl001-it"
		alias  = "api.example.com"
	)
	t.Cleanup(func() {
		c := context.Background()
		_ = e.ServiceRemove(c, svc)
		_ = e.NetworkRemove(c, orgNet)
		_ = e.NetworkRemove(c, egress)
	})
	for _, n := range []string{orgNet, egress} {
		if _, err := e.NetworkEnsure(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	spec := docker.ServiceSpec{Name: svc, Image: "nginx:alpine", Replicas: 0, Network: orgNet, KeepNetworkPrefix: "krill-egress-"}
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	// A later release (the egress one) attached the app to its egress network
	// with a destination alias.
	cur, _, err := cli.ServiceInspectWithRaw(ctx, svc, swarm.ServiceInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Spec.TaskTemplate.Networks = append(cur.Spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: egress, Aliases: []string{alias}})
	if _, err := cli.ServiceUpdate(ctx, cur.ID, cur.Version, cur.Spec, swarm.ServiceUpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	egressID := networkID(t, cli, egress)
	attached := func() (bool, []string) {
		s, _, err := cli.ServiceInspectWithRaw(ctx, svc, swarm.ServiceInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range s.Spec.TaskTemplate.Networks {
			if a.Target == egressID || a.Target == egress {
				return true, a.Aliases
			}
		}
		return false, nil
	}

	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	ok, aliases := attached()
	if !ok || !slices.Equal(aliases, []string{alias}) {
		t.Fatalf("after a redeploy: attached=%v aliases=%v, want the egress attachment with %q", ok, aliases, alias)
	}

	spec.KeepNetworkPrefix = ""
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if ok, _ := attached(); ok {
		t.Fatal("without KeepNetworkPrefix the spec's own networks must win")
	}
}

func networkID(t *testing.T, cli *client.Client, name string) string {
	t.Helper()
	n, err := cli.NetworkInspect(context.Background(), name, network.InspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}
