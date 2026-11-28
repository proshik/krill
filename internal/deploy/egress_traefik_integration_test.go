//go:build integration

package deploy

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// An app on its organization network and its egress network still answers on
// its domain through the pinned Traefik's swarm provider: the
// traefik.docker.network label AppLabels sets picks the organization network
// even though the provider's default is the egress network, which Traefik is
// not on. All objects are krl002-prefixed: the Swarm may be shared.
func TestEgressAppAnswersThroughTraefik(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Nodes(ctx); err != nil {
		t.Skipf("no Swarm: %v", err)
	}
	n := strconv.FormatInt(time.Now().UnixNano()%1e6, 10)
	orgNet, egressNet := "krl002-torg-"+n, "krl002-tegress-"+n
	gwName, appName := "krl002-traefik-"+n, "krl002-tapp-"+n
	host := "web-" + n + ".example.test"
	for _, nw := range []string{orgNet, egressNet} {
		if _, err := eng.NetworkEnsure(ctx, nw); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_ = eng.ServiceRemove(c, gwName)
		_ = eng.ServiceRemove(c, appName)
		for _, nw := range []string{orgNet, egressNet} {
			for c.Err() == nil && eng.NetworkRemove(c, nw) != nil {
				time.Sleep(time.Second)
			}
		}
	})

	d := newDeployer(eng, &mockBuilder{}, newFakeStore(App{}))
	spec := d.buildSpec(App{ID: 990001, Name: "web", Image: "nginx", Tag: "alpine", Port: 80, Network: orgNet,
		LinkEgress: []string{"api.example.test:443"},
		Domains:    []traefik.Domain{{Host: host, Exposed: true, Primary: true}}}, "nginx:alpine")
	if len(spec.Networks) != 2 || spec.Networks[0] != orgNet {
		t.Fatalf("networks %v", spec.Networks)
	}
	spec.Name = appName
	spec.Networks[1] = egressNet
	spec.Labels["krl002.test"] = "1" // only this test's Traefik routes it
	spec.ContainerLabels = nil
	if err := eng.ServiceDeploy(ctx, spec); err != nil {
		t.Fatal(err)
	}

	gw := docker.ServiceSpec{Name: gwName, Image: "traefik:" + traefik.TraefikVersion, Replicas: 1,
		Network: orgNet, Networks: []string{orgNet},
		Args: []string{
			"--providers.swarm.endpoint=unix:///var/run/docker.sock",
			"--providers.swarm.exposedByDefault=false",
			// The default network is one Traefik cannot reach: only the
			// label can route the app.
			"--providers.swarm.network=" + egressNet,
			"--providers.swarm.constraints=Label(`krl002.test`,`1`)",
			"--providers.swarm.refreshSeconds=2",
			"--entrypoints.web.address=:80",
		},
		Mounts:      []docker.MountSpec{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true}},
		Constraints: []string{"node.role==manager"},
	}
	if err := eng.ServiceDeploy(ctx, gw); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	for ctx.Err() == nil {
		out.Reset()
		err := eng.Exec(ctx, gwName, []string{"wget", "-qO-", "-T", "5", "--header", "Host: " + host, "http://127.0.0.1/"}, nil, nil, &out)
		if err == nil && strings.Contains(out.String(), "Welcome to nginx") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("the app on two networks did not answer through Traefik: %q", out.String())
}
