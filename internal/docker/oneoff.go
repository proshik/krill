package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
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
	defer e.removeOneOff(ctx, cid)

	// The attach upgrade and the hijacked stream do raw socket IO that does
	// not observe ctx. This client's connection is closed on cancellation,
	// which also interrupts a daemon that stalls during the handshake.
	attachClient, err := cancelableAttachClient(ctx, cli)
	if err != nil {
		return fmt.Errorf("attach one-off container: %w", err)
	}
	defer attachClient.Close()
	att, err := attachClient.ContainerAttach(ctx, cid, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("attach one-off container: %w", err)
	}
	defer att.Close()
	if err := cli.ContainerStart(ctx, cid, container.StartOptions{}); err != nil {
		return fmt.Errorf("start one-off container: %w", err)
	}
	var stderr cappedBuffer
	if _, err := stdcopy.StdCopy(out, &stderr, &frameReader{r: att.Reader}); err != nil {
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

// frameReader passes Docker's multiplexed attach stream through and turns an
// EOF inside a frame into io.ErrUnexpectedEOF: stdcopy.StdCopy reports a
// stream cut mid-frame as a clean end, and the frame's bytes are then lost
// without an error.
type frameReader struct {
	r    io.Reader
	hdr  int // header bytes of the current frame seen so far
	size [4]byte
	left int // payload bytes of the current frame still to come
}

func (f *frameReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	for b := p[:n]; len(b) > 0; {
		if f.left > 0 {
			k := min(f.left, len(b))
			f.left, b = f.left-k, b[k:]
			continue
		}
		// Header: 1 byte stream, 3 bytes padding, 4 bytes big-endian size.
		if f.hdr >= 4 {
			f.size[f.hdr-4] = b[0]
		}
		f.hdr, b = f.hdr+1, b[1:]
		if f.hdr == 8 {
			f.hdr, f.left = 0, int(binary.BigEndian.Uint32(f.size[:]))
		}
	}
	if errors.Is(err, io.EOF) && (f.hdr > 0 || f.left > 0) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// removeOneOff force-removes a one-off container together with its anonymous
// volumes: an image declaring VOLUME (every postgres image does) gets one at
// creation, and nothing else ever removes it. Named volumes are kept by the
// daemon; a one-off mounts none anyway.
func (e *dockerEngine) removeOneOff(ctx context.Context, cid string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sidecarCleanupTimeout)
	defer cancel()
	err := e.cli.ContainerRemove(rctx, cid, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !errdefs.IsNotFound(err) {
		slog.Warn("could not remove one-off container", "container", cid, "err", err)
	}
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
		// RemoveVolumes: see removeOneOff.
		if err := e.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
