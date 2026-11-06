//go:build integration

package docker_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/proshik/krill/internal/docker"
)

// A network created with an overlay MTU must carry it, and OverlayMTUs must
// read it back; a network created without one reports Docker's default.
func TestOverlayNetworkMTU(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	skipIfNoSwarm(t, e)
	ov, ok := e.(docker.OverlayNetworks)
	if !ok {
		t.Fatal("the real engine must implement OverlayNetworks")
	}
	ctx := context.Background()
	const withMTU, plain = "krill-it-mtu-1370", "krill-it-mtu-default"
	t.Cleanup(func() {
		_ = e.NetworkRemove(context.Background(), withMTU)
		_ = e.NetworkRemove(context.Background(), plain)
	})
	ov.SetOverlayMTU(1370)
	if created, err := e.NetworkEnsure(ctx, withMTU); err != nil || !created {
		t.Fatalf("ensure %s: created=%v err=%v", withMTU, created, err)
	}
	ov.SetOverlayMTU(0)
	if _, err := e.NetworkEnsure(ctx, plain); err != nil {
		t.Fatalf("ensure %s: %v", plain, err)
	}
	mtus, err := ov.OverlayMTUs(ctx)
	if err != nil {
		t.Fatalf("OverlayMTUs: %v", err)
	}
	if mtus[withMTU] != 1370 || mtus[plain] != docker.DefaultOverlayMTU {
		t.Fatalf("mtus: %s=%d %s=%d", withMTU, mtus[withMTU], plain, mtus[plain])
	}
	// What matters is the MTU a container on the network actually gets.
	if got := containerMTU(t, e, withMTU); got != "1370" {
		t.Fatalf("container eth0 MTU on %s = %q, want 1370", withMTU, got)
	}
	// Ensuring an existing network never changes its MTU.
	ov.SetOverlayMTU(1300)
	if created, err := e.NetworkEnsure(ctx, withMTU); err != nil || created {
		t.Fatalf("re-ensure: created=%v err=%v", created, err)
	}
	if mtus, _ := ov.OverlayMTUs(ctx); mtus[withMTU] != 1370 {
		t.Fatalf("existing network changed MTU: %d", mtus[withMTU])
	}
}

// containerMTU runs a throwaway container on network and returns the MTU of its
// interface on it.
func containerMTU(t *testing.T, e docker.Engine, network string) string {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx := context.Background()
	const image = "busybox:1.37.0@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"
	if err := e.ImagePull(ctx, image, io.Discard); err != nil {
		t.Fatalf("pull: %v", err)
	}
	c, err := cli.ContainerCreate(ctx, &container.Config{Image: image, Cmd: []string{"cat", "/sys/class/net/eth0/mtu"}},
		&container.HostConfig{NetworkMode: container.NetworkMode(network)}, nil, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cli.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true})
	if err := cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	wait, errc := cli.ContainerWait(ctx, c.ID, container.WaitConditionNotRunning)
	select {
	case <-wait:
	case err := <-errc:
		t.Fatalf("wait: %v", err)
	}
	logs, err := cli.ContainerLogs(ctx, c.ID, container.LogsOptions{ShowStdout: true})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer logs.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, io.Discard, logs); err != nil {
		t.Fatalf("read logs: %v", err)
	}
	return strings.TrimSpace(out.String())
}
