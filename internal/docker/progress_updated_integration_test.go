//go:build integration

package docker_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/proshik/krill/internal/docker"
)

// ServiceProgress reports when the service object last changed, and a scale
// to zero moves it: the status heal takes a service unchanged at zero for an
// hour for a stopped app, so the time must be the scale's, not the creation's.
func TestServiceProgressUpdatedAtFollowsAScale(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	skipIfNoSwarm(t, e)
	ctx := context.Background()
	svc := "krill-it-updated-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { _ = e.ServiceRemove(context.Background(), svc) })

	spec := docker.ServiceSpec{Name: svc, Image: "busybox:1.36", Replicas: 1, Network: "krill-net", Command: []string{"sleep", "3600"}}
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	created, err := e.ServiceProgress(ctx, svc, nil)
	if err != nil {
		t.Fatalf("ServiceProgress: %v", err)
	}
	if created.UpdatedAt.IsZero() {
		t.Fatal("ServiceProgress.UpdatedAt is zero for an existing service")
	}
	time.Sleep(1100 * time.Millisecond)
	before := time.Now()
	if err := e.ServiceScale(ctx, svc, 0); err != nil {
		t.Fatalf("scale: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		p, err := e.ServiceProgress(ctx, svc, nil)
		if err != nil {
			t.Fatalf("ServiceProgress: %v", err)
		}
		if p.Desired == 0 && p.Running == 0 {
			if !p.UpdatedAt.After(created.UpdatedAt) || p.UpdatedAt.Before(before.Add(-2*time.Second)) {
				t.Fatalf("UpdatedAt after the scale = %v, created %v, scaled at %v", p.UpdatedAt, created.UpdatedAt, before)
			}
			// It is the service object's own time, not the time of the read:
			// it equals what Docker reports and does not move while nothing
			// changes.
			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatalf("docker client: %v", err)
			}
			defer cli.Close()
			raw, _, err := cli.ServiceInspectWithRaw(ctx, svc, swarm.ServiceInspectOptions{})
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if !p.UpdatedAt.Equal(raw.UpdatedAt) {
				t.Fatalf("UpdatedAt = %v, Docker's service UpdatedAt = %v", p.UpdatedAt, raw.UpdatedAt)
			}
			time.Sleep(1100 * time.Millisecond)
			again, err := e.ServiceProgress(ctx, svc, nil)
			if err != nil {
				t.Fatalf("ServiceProgress: %v", err)
			}
			if !again.UpdatedAt.Equal(p.UpdatedAt) {
				t.Fatalf("UpdatedAt moved without a change: %v then %v", p.UpdatedAt, again.UpdatedAt)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not scale to zero: %+v", p)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
