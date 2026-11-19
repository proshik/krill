package main

import (
	"bytes"
	"context"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
)

// localNodeSource is the control-plane node's stats source: the local docker
// engine, plus disk usage from df on this host (Krill runs as a host binary
// there; inside a container without df the snapshot just fails and is
// skipped).
type localNodeSource struct{ docker.Engine }

func (localNodeSource) DiskUsage(ctx context.Context, paths []string) ([]metrics.DiskUsage, error) {
	return metrics.DiskViaDF(ctx, metrics.LocalRun, paths)
}

// workerNodeSource is a worker's stats source: its docker socket tunnelled
// over SSH, and df run over the same SSH connection.
type workerNodeSource struct {
	*docker.RemoteStats
	ssh *ssh.Client
}

func (w workerNodeSource) DiskUsage(ctx context.Context, paths []string) ([]metrics.DiskUsage, error) {
	return metrics.DiskViaDF(ctx, w.run, paths)
}

// run runs argv on the worker; ctx cancellation closes the session, which
// ends the remote command.
func (w workerNodeSource) run(ctx context.Context, argv []string) ([]byte, error) {
	sess, err := w.ssh.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = metrics.ShellQuote(a)
	}
	var out bytes.Buffer
	sess.Stdout = &out
	done := make(chan error, 1)
	go func() { done <- sess.Run("LC_ALL=C " + strings.Join(quoted, " ")) }()
	select {
	case <-ctx.Done():
		_ = sess.Close()
		<-done // the session is closed: Run returns, and out is no longer written
		return nil, ctx.Err()
	case err := <-done:
		return out.Bytes(), err
	}
}
