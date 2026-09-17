//go:build integration

package docker_test

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/proshik/krill/internal/docker"
)

func TestSwarmObjectsLifecycle(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	skipIfNoSwarm(t, e)
	objs, ok := e.(docker.SwarmObjects)
	if !ok {
		t.Fatal("engine does not implement SwarmObjects")
	}
	ctx := context.Background()
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	labelKey, labelVal := "krill.it-objects", sfx
	labels := map[string]string{labelKey: labelVal}
	cfg, sec, svc := "krill-it-cfg-"+sfx, "krill-it-sec-"+sfx, "krill-it-files-"+sfx
	t.Cleanup(func() {
		_ = e.ServiceRemove(context.Background(), svc)
		time.Sleep(3 * time.Second)
		_ = objs.PruneObjects(context.Background(), labelKey, labelVal, nil)
	})

	for i := 0; i < 2; i++ { // the second round proves Ensure is idempotent
		if err := objs.ConfigEnsure(ctx, cfg, []byte("hello-config"), labels); err != nil {
			t.Fatalf("ConfigEnsure #%d: %v", i, err)
		}
		if err := objs.SecretEnsure(ctx, sec, []byte("hello-secret"), labels); err != nil {
			t.Fatalf("SecretEnsure #%d: %v", i, err)
		}
	}

	spec := docker.ServiceSpec{
		Name: svc, Image: "busybox:1.36", Replicas: 1, Network: "krill-net",
		Command:         []string{"sh", "-c", "cat /etc/it.conf; echo; cat /run/secrets/it_pw; echo; hostname; sleep 3600"},
		Configs:         []docker.FileRef{{Name: cfg, Target: "/etc/it.conf"}},
		Secrets:         []docker.FileRef{{Name: sec, Target: "it_pw", Mode: 0o400}},
		Hostname:        "{{.Node.Hostname}}",
		ContainerLabels: map[string]string{"krill.it": "yes"},
		UpdateStopFirst: true,
	}
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		p, err := e.ServiceProgress(ctx, svc, nil)
		if err == nil && p.Running >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not start: %+v %v", p, err)
		}
		time.Sleep(time.Second)
	}
	time.Sleep(2 * time.Second)
	rc, err := e.ServiceLogs(ctx, svc, false, 50)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	for _, want := range []string{"hello-config", "hello-secret"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("logs lack %q: %q", want, b)
		}
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	tasks, err := cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", svc))})
	if err != nil || len(tasks) == 0 {
		t.Fatalf("task list: %v %d", err, len(tasks))
	}
	if tasks[0].Spec.ContainerSpec.Labels["krill.it"] != "yes" {
		t.Errorf("container label missing: %v", tasks[0].Spec.ContainerSpec.Labels)
	}

	// In use: pruning everything is not an error and removes nothing.
	if err := objs.PruneObjects(ctx, labelKey, labelVal, nil); err != nil {
		t.Fatalf("prune while in use: %v", err)
	}
	count := func() int {
		f := filters.NewArgs(filters.Arg("label", labelKey+"="+labelVal))
		c, _ := cli.ConfigList(ctx, swarm.ConfigListOptions{Filters: f})
		s, _ := cli.SecretList(ctx, swarm.SecretListOptions{Filters: f})
		return len(c) + len(s)
	}
	if n := count(); n != 2 {
		t.Fatalf("objects after in-use prune = %d, want 2", n)
	}

	if err := e.ServiceRemove(ctx, svc); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(60 * time.Second)
	for count() != 0 {
		if err := objs.PruneObjects(ctx, labelKey, labelVal, nil); err != nil {
			t.Fatalf("prune after remove: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("objects not pruned after the service was removed: %d left", count())
		}
		time.Sleep(2 * time.Second)
	}

	// A missing object fails the deploy with a clear error.
	bad := spec
	bad.Name = svc + "-bad"
	bad.Configs = []docker.FileRef{{Name: "krill-it-missing-" + sfx, Target: "/x"}}
	if err := e.ServiceDeploy(ctx, bad); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("deploy with a missing config: err = %v", err)
	}
}
