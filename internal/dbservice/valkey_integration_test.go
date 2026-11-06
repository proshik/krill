//go:build integration

package dbservice_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/proshik/krill/internal/dbservice/drivers"
)

// TestValkeyDriverOnRealImage runs the exact spec the valkey driver builds,
// as plain containers sharing one named volume, and checks what the driver
// promises: the server drops to the valkey user (it would stay root with the
// redis driver's args), the healthcheck command passes with the password from
// the environment and fails without it, data written survives a new container
// on the same volume (append-only file), and a second container on the volume
// waits on the directory lock instead of opening the data alongside the first.
//
// Run against the default image and an alpine tag: the image is free text at
// create time, and alpine ships busybox's flock.
func TestValkeyDriverOnRealImage(t *testing.T) {
	d, err := drivers.Registry.Lookup("valkey")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{d.DefaultImage(), "valkey/valkey:8.1.4-alpine"} {
		t.Run(image, func(t *testing.T) { testValkeyDriver(t, d, image) })
	}
}

func testValkeyDriver(t *testing.T, d drivers.Driver, image string) {
	ctx := context.Background()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	spec := d.BuildSpec(drivers.Instance{Engine: "valkey", AppName: "krill-it-valkey", Image: image, SuperuserPassword: "pw-it"}, "unused")
	envs := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		envs = append(envs, k+"="+v)
	}
	suffix := sanitizeForDockerName(image)
	volName := "krill-it-valkey-data-" + suffix
	ensureImagePresent(t, ctx, cli, spec.Image)
	_ = cli.VolumeRemove(ctx, volName, true)
	if _, err := cli.VolumeCreate(ctx, volume.CreateOptions{Name: volName}); err != nil {
		t.Fatalf("volume create: %v", err)
	}
	t.Cleanup(func() { _ = cli.VolumeRemove(context.Background(), volName, true) })

	start := func(name string) string {
		t.Helper()
		_ = cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
		resp, err := cli.ContainerCreate(ctx,
			&container.Config{Image: spec.Image, Entrypoint: spec.Command, Cmd: spec.Args, Env: envs},
			&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volName, Target: d.MountTarget()}}},
			nil, nil, name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true}) })
		if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		return resp.ID
	}
	const ready = "Ready to accept connections"

	first := start("krill-it-valkey-a-" + suffix)
	waitForLogOccurrences(t, ctx, cli, first, ready, 1, 60*time.Second)

	if out, code := execIn(t, ctx, cli, first, nil, "sh", "-c", "stat -c %U /proc/1"); code != 0 || strings.TrimSpace(out) != "valkey" {
		t.Fatalf("server must run as the valkey user, got %q (exit %d)", out, code)
	}
	health := spec.Healthcheck.Test[1]
	if out, code := execIn(t, ctx, cli, first, nil, "sh", "-c", health); code != 0 {
		t.Fatalf("healthcheck %q failed (exit %d): %s", health, code, out)
	}
	if _, code := execIn(t, ctx, cli, first, []string{"REDISCLI_AUTH="}, "sh", "-c", health); code == 0 {
		t.Fatal("the healthcheck must fail without the password: it would report a server that refuses clients healthy")
	}
	if out, code := execIn(t, ctx, cli, first, nil, "valkey-cli", "SET", "krill-it", "kept"); code != 0 || !strings.Contains(out, "OK") {
		t.Fatalf("SET: %q (exit %d)", out, code)
	}

	// A second container on the same volume must wait on the lock.
	second := start("krill-it-valkey-b-" + suffix)
	time.Sleep(5 * time.Second)
	if logs := fetchLogs(t, ctx, cli, second); strings.Contains(logs, ready) {
		t.Fatalf("second server started while the first holds the volume:\n%s", logs)
	}

	if err := cli.ContainerStop(ctx, first, container.StopOptions{}); err != nil {
		t.Fatalf("stop first: %v", err)
	}
	waitForContainerExited(t, ctx, cli, first, 30*time.Second)
	waitForLogOccurrences(t, ctx, cli, second, ready, 1, 60*time.Second)
	if out, code := execIn(t, ctx, cli, second, nil, "valkey-cli", "GET", "krill-it"); code != 0 || strings.TrimSpace(out) != "kept" {
		t.Fatalf("data must survive a new container on the volume, GET = %q (exit %d)", out, code)
	}
}

// execIn runs cmd in a container (env added to the container's own) and
// returns its combined output and exit code.
func execIn(t *testing.T, ctx context.Context, cli *dockerclient.Client, id string, env []string, cmd ...string) (string, int) {
	t.Helper()
	ex, err := cli.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: cmd, Env: env, AttachStdout: true, AttachStderr: true})
	if err != nil {
		t.Fatalf("exec create: %v", err)
	}
	att, err := cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach: %v", err)
	}
	defer att.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil {
		t.Fatalf("exec read: %v", err)
	}
	insp, err := cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		t.Fatalf("exec inspect: %v", err)
	}
	return out.String(), insp.ExitCode
}
