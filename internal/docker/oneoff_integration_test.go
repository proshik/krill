//go:build integration

package docker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

func oneOffEngine(t *testing.T) *dockerEngine {
	t.Helper()
	eng, err := NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	e, ok := eng.(*dockerEngine)
	if !ok {
		t.Fatalf("expected *dockerEngine, got %T", eng)
	}
	if _, ok := eng.(OneOffRunner); !ok {
		t.Fatalf("engine does not implement OneOffRunner")
	}
	return e
}

func labelled(t *testing.T, e *dockerEngine, value string) int {
	t.Helper()
	list, err := e.cli.ContainerList(context.Background(), container.ListOptions{
		All: true, Filters: filters.NewArgs(filters.Arg("label", OneOffRoleLabel+"="+value)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func TestRunOneOffStreamsStdout(t *testing.T) {
	e := oneOffEngine(t)
	var out bytes.Buffer
	err := e.RunOneOff(context.Background(), OneOffSpec{
		Image:  busyboxImage,
		Cmd:    []string{"sh", "-c", "printf hello; echo oops >&2"},
		Labels: map[string]string{OneOffRoleLabel: "oneoff-stdout"},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" {
		t.Fatalf("out = %q", out.String())
	}
	if n := labelled(t, e, "oneoff-stdout"); n != 0 {
		t.Fatalf("%d containers remain", n)
	}
}

func TestRunOneOffNonZeroExit(t *testing.T) {
	e := oneOffEngine(t)
	err := e.RunOneOff(context.Background(), OneOffSpec{
		Image: busyboxImage, Cmd: []string{"sh", "-c", "echo bad >&2; exit 3"},
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exited 3") || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v", err)
	}
	err = e.RunOneOff(context.Background(), OneOffSpec{
		Image: busyboxImage,
		Cmd:   []string{"sh", "-c", "head -c 4096 /dev/zero | tr '\\0' 'Q' >&2; exit 1"},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error")
	}
	if n := strings.Count(err.Error(), "Q"); n != 1024 {
		t.Fatalf("stderr in error = %d bytes, want 1024", n)
	}
}

func TestRunOneOffEnvNotInError(t *testing.T) {
	e := oneOffEngine(t)
	err := e.RunOneOff(context.Background(), OneOffSpec{
		Image: busyboxImage, Env: []string{"SECRET_X=topsecret"}, Cmd: []string{"sh", "-c", "exit 1"},
	}, &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunOneOffReadOnlyFile(t *testing.T) {
	e := oneOffEngine(t)
	dir, err := os.MkdirTemp(os.Getenv("HOME"), "krill-oneoff-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(f, []byte("pem-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := e.RunOneOff(context.Background(), OneOffSpec{
		Image: busyboxImage, Cmd: []string{"cat", f}, ReadOnlyFiles: []string{f},
	}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "pem-content" {
		t.Fatalf("out = %q", out.String())
	}
	if err := e.RunOneOff(context.Background(), OneOffSpec{
		Image: busyboxImage, Cmd: []string{"sh", "-c", "echo x > " + f}, ReadOnlyFiles: []string{f},
	}, &bytes.Buffer{}); err == nil {
		t.Fatal("write to a read-only mount succeeded")
	}
}

func TestRunOneOffHostNetwork(t *testing.T) {
	e := oneOffEngine(t)
	var out bytes.Buffer
	if err := e.RunOneOff(context.Background(), OneOffSpec{
		Image: busyboxImage, Cmd: []string{"ls", "/sys/class/net"}, HostNetwork: true,
	}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "docker0") && !strings.Contains(out.String(), "docker_gwbridge") {
		t.Skipf("host netns lists no docker0/docker_gwbridge (%q); cannot prove host networking here", out.String())
	}
}

func TestRemoveContainersByLabel(t *testing.T) {
	e := oneOffEngine(t)
	ctx := context.Background()
	if err := ensureImageOn(ctx, e.cli, busyboxImage); err != nil {
		t.Fatal(err)
	}
	resp, err := e.cli.ContainerCreate(ctx, &container.Config{
		Image: busyboxImage, Cmd: []string{"sleep", "300"},
		Labels: map[string]string{OneOffRoleLabel: "state-dump-test"},
	}, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer e.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	if err := e.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	n, err := e.RemoveContainersByLabel(ctx, OneOffRoleLabel, "state-dump-test")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if c := labelled(t, e, "state-dump-test"); c != 0 {
		t.Fatalf("%d remain", c)
	}
}

func TestRunOneOffCancelRemovesContainer(t *testing.T) {
	e := oneOffEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	err := e.RunOneOff(ctx, OneOffSpec{
		Image: busyboxImage, Cmd: []string{"sleep", "60"},
		Labels: map[string]string{OneOffRoleLabel: "oneoff-cancel"},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error")
	}
	deadline := time.Now().Add(15 * time.Second)
	for labelled(t, e, "oneoff-cancel") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("container remains after cancel")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// runWatched runs spec (labelled value) and inspects its container while it
// is still running; spec's command must stay up for a moment.
func runWatched(t *testing.T, e *dockerEngine, spec OneOffSpec, value string) (container.InspectResponse, string, error) {
	t.Helper()
	ctx := context.Background()
	spec.Labels = map[string]string{OneOffRoleLabel: value}
	var out bytes.Buffer
	errCh := make(chan error, 1)
	go func() { errCh <- e.RunOneOff(ctx, spec, &out) }()
	var seen container.InspectResponse
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the one-off container never appeared")
		}
		list, err := e.cli.ContainerList(ctx, container.ListOptions{
			All: true, Filters: filters.NewArgs(filters.Arg("label", OneOffRoleLabel+"="+value)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 {
			if seen, err = e.cli.ContainerInspect(ctx, list[0].ID); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	err := <-errCh
	return seen, out.String(), err
}

func TestRunOneOffKeepsNoLog(t *testing.T) {
	e := oneOffEngine(t)
	seen, out, err := runWatched(t, e, OneOffSpec{
		Image: busyboxImage, Cmd: []string{"sh", "-c", "sleep 1; printf hello"},
	}, "oneoff-nolog")
	if err != nil {
		t.Fatal(err)
	}
	if seen.HostConfig == nil {
		t.Fatal("inspect returned no host config")
	}
	if got := seen.HostConfig.LogConfig.Type; got != "none" {
		t.Fatalf("log driver = %q, want none: the daemon would keep a copy of the output", got)
	}
	if out != "hello" {
		t.Fatalf("out = %q: attach must still deliver the output", out)
	}
}
