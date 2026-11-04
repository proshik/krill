package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/proshik/krill/internal/docker"
)

const predeployTimeout = 5 * time.Minute

func (d *Deployer) predeploy(ctx context.Context, app App, deploymentID int64, image string, out io.Writer) error {
	runner, ok := d.engine.(docker.PredeployRunner)
	if !ok {
		return errors.New("Docker engine does not support predeploy commands")
	}
	app = d.withResourceDefaults(app)
	network := app.Network
	if network == "" {
		network = d.network
	}
	pctx, cancel := context.WithTimeout(ctx, predeployTimeout)
	defer cancel()
	masked := newSecretWriter(out, app.Env)
	fmt.Fprintln(out, "→ running predeploy command...")
	err := runner.RunPredeploy(pctx, docker.PredeploySpec{
		AppID: app.ID, DeploymentID: deploymentID, Image: image, RegistryAuth: app.RegistryAuth,
		Command: app.PredeployCommand, Env: app.Env, Network: network,
		PlacementMode: app.PlacementMode, PlacementNodes: app.PlacementNodes,
		MemoryLimitBytes: app.MemoryLimitBytes, NanoCPUs: app.NanoCPUs,
	}, masked)
	flushErr := masked.Flush()
	if err != nil {
		// Engine errors may include remote output. Keep command text and env out of
		// notifications as well as the member-visible deployment log.
		msg := strings.ReplaceAll(err.Error(), app.PredeployCommand, "[command]")
		for _, v := range masked.values {
			msg = strings.ReplaceAll(msg, v, "[redacted]")
		}
		return errors.New(msg)
	}
	if flushErr != nil {
		return fmt.Errorf("write predeploy logs: %w", flushErr)
	}
	fmt.Fprintln(out, "→ predeploy completed")
	return nil
}

// secretWriter holds a suffix until it cannot be part of a secret split across
// writes. All env values are treated conservatively as secrets, including DB
// links and metrics tokens. It retains at most the longest value plus a chunk.
// One Docker log copier owns this writer; stdout/stderr are demultiplexed serially.
type secretWriter struct {
	out     io.Writer
	values  []string
	pending string
	max     int
}

func newSecretWriter(out io.Writer, env map[string]string) *secretWriter {
	w := &secretWriter{out: out, max: 1}
	seen := map[string]bool{}
	for _, v := range env {
		if v != "" && !seen[v] {
			seen[v] = true
			w.values = append(w.values, v)
			if len(v) > w.max {
				w.max = len(v)
			}
		}
	}
	sort.Slice(w.values, func(i, j int) bool { return len(w.values[i]) > len(w.values[j]) })
	return w
}
func (w *secretWriter) Write(p []byte) (int, error) {
	w.pending += string(p)
	if err := w.emit(false); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (w *secretWriter) Flush() error { return w.emit(true) }
func (w *secretWriter) emit(final bool) error {
	var b strings.Builder
	for len(w.pending) > 0 && (final || len(w.pending) >= w.max) {
		match := ""
		for _, v := range w.values {
			if strings.HasPrefix(w.pending, v) {
				match = v
				break
			}
		}
		if match != "" {
			b.WriteString("[redacted]")
			w.pending = w.pending[len(match):]
		} else {
			b.WriteByte(w.pending[0])
			w.pending = w.pending[1:]
		}
	}
	if b.Len() == 0 {
		return nil
	}
	_, err := io.WriteString(w.out, b.String())
	return err
}
