package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
)

// OneOffRoleLabel marks helper containers started by RunOneOff so a leftover
// one (after a crash) can be found and removed by RemoveContainersByLabel.
const OneOffRoleLabel = "krill.role"

// oneOffStderrCap bounds the stderr text carried in an error.
const oneOffStderrCap = 1024

// OneOffSpec describes a throwaway container on the local daemon.
type OneOffSpec struct {
	Image         string
	Cmd           []string
	Env           []string // never logged and never part of an error
	HostNetwork   bool
	ReadOnlyFiles []string // host paths bind-mounted read-only at the same path
	Labels        map[string]string
}

// OneOffRunner is an optional capability of the real engine (type-assert it).
type OneOffRunner interface {
	// RunOneOff runs spec to completion, streaming stdout to out. A non-zero
	// exit is an error carrying at most 1 KiB of stderr.
	RunOneOff(ctx context.Context, spec OneOffSpec, out io.Writer) error
	// RemoveContainersByLabel force-removes every container (running or not)
	// carrying key=value and returns how many it removed.
	RemoveContainersByLabel(ctx context.Context, key, value string) (int, error)
}

var _ OneOffRunner = (*dockerEngine)(nil)

// cappedBuffer keeps only the first oneOffStderrCap bytes written to it.
type cappedBuffer struct{ buf bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := oneOffStderrCap - c.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf.Write(p[:room])
	}
	return len(p), nil
}

func (e *dockerEngine) RunOneOff(ctx context.Context, spec OneOffSpec, out io.Writer) error {
	cli := e.cli
	if err := ensureImageOn(ctx, cli, spec.Image); err != nil {
		return fmt.Errorf("prepare image %s: %w", spec.Image, err)
	}
	// Output is read only through attach. A logging driver would keep a
	// second, unencrypted copy of it (json-file writes it to disk).
	host := &container.HostConfig{NetworkMode: "none", LogConfig: container.LogConfig{Type: "none"}}
	if spec.HostNetwork {
		host.NetworkMode = "host"
	}
	for _, f := range spec.ReadOnlyFiles {
		host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: f, Target: f, ReadOnly: true})
	}
	resp, err := cli.ContainerCreate(ctx,
		&container.Config{Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, Labels: spec.Labels},
		host, nil, nil, "")
	if err != nil {
		return fmt.Errorf("create one-off container: %w", err)
	}
	cid := resp.ID
	defer e.removeSidecarOn(ctx, "", cid)

	att, err := cli.ContainerAttach(ctx, cid, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return fmt.Errorf("attach one-off container: %w", err)
	}
	defer att.Close()
	// The hijacked stream does not observe ctx; close it on cancellation so
	// the copy below returns.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			att.Close()
		case <-stop:
		}
	}()
	if err := cli.ContainerStart(ctx, cid, container.StartOptions{}); err != nil {
		return fmt.Errorf("start one-off container: %w", err)
	}
	var stderr cappedBuffer
	if _, err := stdcopy.StdCopy(out, &stderr, att.Reader); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read one-off container output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return waitContainer(ctx, cli, cid, "one-off container", &stderr.buf)
}

func (e *dockerEngine) RemoveContainersByLabel(ctx context.Context, key, value string) (int, error) {
	list, err := e.cli.ContainerList(ctx, container.ListOptions{
		All: true, Filters: filters.NewArgs(filters.Arg("label", key+"="+value)),
	})
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, c := range list {
		if err := e.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
