//go:build integration

package dbservice_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/proshik/krill/internal/dbservice/drivers"
)

// TestDirectoryLockPreventsTwoPostmasters reproduces, at the mechanism level,
// the live-cluster incident this fix exists to close: two Postgres
// postmasters running against the same PGDATA at once (caught during a
// worker-node return, ~0.5s of overlap). It runs two plain containers — no
// Swarm involved, matching the real bug: it doesn't matter whether Swarm or a
// human hand produced the second container — with the EXACT Command the
// postgres driver now wraps its entrypoint in
// (drivers.Registry.MustGet("postgres").BuildSpec), sharing one named
// volume, and proves both halves of the mechanism:
//
//  1. while the first container holds the flock on the volume's mount-point
//     directory, the second container's postmaster never starts — it stays
//     silent (no log output at all) and RUNNING (blocked inside flock, not
//     crashed or exited).
//  2. once the first container is stopped CLEANLY (its own log must show a
//     clean shutdown, proving `--no-fork` really forwards the stop signal
//     instead of leaving flock as PID 1 to swallow it), the second
//     container's postmaster starts and its own log carries NO "was not
//     properly shut down" line — i.e. it never raced the first one for the
//     data directory.
func TestDirectoryLockPreventsTwoPostmasters(t *testing.T) {
	ctx := context.Background()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}

	d := drivers.Registry.MustGet("postgres")
	spec := d.BuildSpec(drivers.Instance{
		Engine: "postgres", AppName: "krill-it-lock", Image: "postgres:17",
		Superuser: "postgres", SuperuserPassword: "pw",
	}, "unused-network")
	if len(spec.Command) == 0 {
		t.Fatal("postgres driver spec has no Command — the directory-lock wrapper is missing")
	}
	envs := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		envs = append(envs, k+"="+v)
	}

	// A named volume (not a bind mount): Colima only shares $HOME into the VM,
	// so a bind mount of a host temp dir would not be visible to the daemon.
	const volName = "krill-it-lock-data"
	ensureImagePresent(t, ctx, cli, spec.Image)
	if _, err := cli.VolumeCreate(ctx, volume.CreateOptions{Name: volName}); err != nil {
		t.Fatalf("volume create: %v", err)
	}
	t.Cleanup(func() { _ = cli.VolumeRemove(context.Background(), volName, true) })

	newLockedContainer := func(name string) string {
		t.Helper()
		resp, err := cli.ContainerCreate(ctx,
			&container.Config{Image: spec.Image, Entrypoint: spec.Command, Cmd: spec.Args, Env: envs},
			&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volName, Target: d.MountTarget()}}},
			nil, nil, name)
		if err != nil {
			t.Fatalf("container create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true}) })
		if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
			t.Fatalf("container start %s: %v", name, err)
		}
		return resp.ID
	}

	// The first container initializes a fresh volume: postgres's own
	// entrypoint logs "ready" twice — once for the temporary single-user-mode
	// server it starts to run init tasks, again for the real server it execs
	// afterward.
	firstID := newLockedContainer("krill-it-lock-a")
	waitForLogOccurrences(t, ctx, cli, firstID, "database system is ready to accept connections", 2, 60*time.Second)

	secondID := newLockedContainer("krill-it-lock-b")

	// (a) While the first container holds the lock, the second's postmaster
	// must NOT start: no log output at all (flock itself writes nothing, and
	// nothing after it in the chain runs until flock returns), and the
	// container must still be RUNNING — not exited, not crash-looping —
	// because flock --no-fork is blocked as PID 1, not failing.
	time.Sleep(5 * time.Second)
	if logs := fetchLogs(t, ctx, cli, secondID); logs != "" {
		t.Fatalf("second container produced output while the first still holds the lock (its postmaster started concurrently with the first's):\n%s", logs)
	}
	insp, err := cli.ContainerInspect(ctx, secondID)
	if err != nil {
		t.Fatalf("inspect second container: %v", err)
	}
	if insp.State == nil || !insp.State.Running {
		t.Fatalf("second container is not running while it should be blocked on the lock: state=%+v", insp.State)
	}

	// Stop the first container. The postgres image's own STOPSIGNAL is SIGINT
	// ("fast shutdown"), inherited here since we didn't override
	// Config.StopSignal — docker's default ContainerStop then sends exactly
	// that signal to PID 1 of the container.
	if err := cli.ContainerStop(ctx, firstID, container.StopOptions{}); err != nil {
		t.Fatalf("stop first container: %v", err)
	}
	waitForContainerExited(t, ctx, cli, firstID, 30*time.Second)
	firstLogs := fetchLogs(t, ctx, cli, firstID)
	if !strings.Contains(firstLogs, "database system is shut down") {
		t.Fatalf("first container did not shut down cleanly — no \"database system is shut down\" in its log; "+
			"this means --no-fork did NOT forward the stop signal to postgres (flock stayed PID 1) and the daemon "+
			"had to SIGKILL it instead:\n%s", firstLogs)
	}

	// (b) The volume is free now. The second container's postmaster must
	// start, and cleanly — no crash recovery, because the first one had
	// genuinely finished (not merely appeared to, from outside) before the
	// second acquired the lock. Unlike the first container, the volume is
	// already initialized here, so postgres skips the init-task temp server
	// entirely and logs "ready" only ONCE.
	waitForLogOccurrences(t, ctx, cli, secondID, "database system is ready to accept connections", 1, 60*time.Second)
	secondLogs := fetchLogs(t, ctx, cli, secondID)
	if strings.Contains(secondLogs, "was not properly shut down") {
		t.Fatalf("second container ran crash recovery on start — it saw a data directory the first container had not "+
			"actually finished with when the lock was acquired:\n%s", secondLogs)
	}
	t.Logf("confirmed: the second container waited out the lock while the first held it, and started cleanly (no crash recovery) once the first released it")
}

func ensureImagePresent(t *testing.T, ctx context.Context, cli *dockerclient.Client, ref string) {
	t.Helper()
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return
	}
	rc, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		t.Skipf("cannot pull %s: %v", ref, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
}

// fetchLogs returns a container's full stdout+stderr log as one demuxed
// string (the raw stream from ContainerLogs is stdcopy-multiplexed since the
// container has no TTY).
func fetchLogs(t *testing.T, ctx context.Context, cli *dockerclient.Client, containerID string) string {
	t.Helper()
	rc, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("logs %s: %v", containerID, err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil && err != io.EOF {
		t.Fatalf("demux logs %s: %v", containerID, err)
	}
	return buf.String()
}

func waitForLogOccurrences(t *testing.T, ctx context.Context, cli *dockerclient.Client, containerID, substr string, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Count(fetchLogs(t, ctx, cli, containerID), substr) >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s log never reached %d occurrence(s) of %q within %s", containerID, count, substr, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitForContainerExited(t *testing.T, ctx context.Context, cli *dockerclient.Client, containerID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		insp, err := cli.ContainerInspect(ctx, containerID)
		if err == nil && insp.State != nil && !insp.State.Running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s did not exit within %s", containerID, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
