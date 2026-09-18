package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// Engine is what Reconcile needs from Docker. The real engine satisfies it
// only when it also implements docker.SwarmObjects.
type Engine interface {
	NetworkEnsure(ctx context.Context, name string) (bool, error)
	ServiceDeploy(ctx context.Context, spec docker.ServiceSpec) error
	ServiceRemove(ctx context.Context, name string) error
	ServiceState(ctx context.Context, name string) (docker.ServiceState, error)
	ServiceProgress(ctx context.Context, name string, exclude []string) (docker.ServiceProgress, error)
	ServiceLabels(ctx context.Context, name string) (map[string]string, bool, error)
	docker.SwarmObjects
}

// convergeTimeout covers pulling the agent image on every node.
var (
	convergeTimeout = 5 * time.Minute
	convergePoll    = 2 * time.Second
)

// Reconcile converges the agent to s: deployed with this configuration when
// enabled, absent otherwise. nodes maps Swarm hostnames to the names Krill
// shows for them (see RenderNodeConfig); it is unused when s is disabled.
func Reconcile(ctx context.Context, eng Engine, s Settings, network string, nodes []NodeName) error {
	if !s.Enabled {
		return teardown(ctx, eng)
	}
	cfg, err := RenderNodeConfig(s, nodes)
	if err != nil {
		return err
	}
	obj := objectsOf(s, cfg)
	labels := map[string]string{ObjectLabel: ObjectLabelValue}
	if err := eng.ConfigEnsure(ctx, obj.Config, cfg, labels); err != nil {
		return fmt.Errorf("create the agent configuration: %w", err)
	}
	if obj.MetricsSecret != "" {
		if err := eng.SecretEnsure(ctx, obj.MetricsSecret, []byte(s.Metrics.Password), labels); err != nil {
			return fmt.Errorf("create the metrics password secret: %w", err)
		}
	}
	if obj.LogsSecret != "" {
		if err := eng.SecretEnsure(ctx, obj.LogsSecret, []byte(s.Logs.Password), labels); err != nil {
			return fmt.Errorf("create the logs password secret: %w", err)
		}
	}
	netCreated, err := eng.NetworkEnsure(ctx, network)
	if err != nil {
		return err
	}
	spec := NodeSpec(obj, network)
	before, err := eng.ServiceProgress(ctx, NodeServiceName, nil)
	if err != nil {
		return fmt.Errorf("read the agent's tasks: %w", err)
	}
	// A matching label proves the spec was written, not that it finished
	// rolling out; an unsettled update is deployed again. A network that was
	// just (re)created has the same name but is new to Swarm, so the service
	// is deployed again to attach to it.
	if before.Found && !netCreated && docker.UpdateSettled(before.UpdateState) {
		if cur, found, err := eng.ServiceLabels(ctx, NodeServiceName); err == nil && found {
			if h := spec.Labels[specHashLabel]; h != "" && cur[specHashLabel] == h {
				prune(ctx, eng, obj)
				return nil
			}
		}
	}
	var baseline []string
	if before.Found {
		baseline = before.TaskIDs
	}
	slog.Info("deploying the observability agent",
		"metrics", s.Metrics.Configured(), "logs", s.Logs.Configured())
	if err := eng.ServiceDeploy(ctx, spec); err != nil {
		return fmt.Errorf("deploy the agent: %w", err)
	}
	if err := waitAgent(ctx, eng, baseline); err != nil {
		return err
	}
	slog.Info("observability agent deployed")
	prune(ctx, eng, obj)
	return nil
}

func teardown(ctx context.Context, eng Engine) error {
	_, found, err := eng.ServiceLabels(ctx, NodeServiceName)
	if err != nil {
		return fmt.Errorf("read the agent service: %w", err)
	}
	if found {
		if err := eng.ServiceRemove(ctx, NodeServiceName); err != nil {
			return fmt.Errorf("remove the agent: %w", err)
		}
		slog.Info("observability agent removed")
	}
	// Objects the stopping tasks still mount are skipped now and removed by
	// the next pass — at the latest on the next start.
	prune(ctx, eng, Objects{})
	return nil
}

// prune drops the objects of earlier specs. A failure only leaves garbage
// behind, so it is logged, not returned.
func prune(ctx context.Context, eng Engine, obj Objects) {
	var keep []string
	for _, n := range []string{obj.Config, obj.MetricsSecret, obj.LogsSecret} {
		if n != "" {
			keep = append(keep, n)
		}
	}
	if err := eng.PruneObjects(ctx, ObjectLabel, ObjectLabelValue, keep); err != nil {
		slog.Warn("observability: could not remove old agent configs and secrets", "err", err)
	}
}

// waitAgent polls until the agent's new tasks run on every node and Swarm has
// completed the update. A node that is down keeps its task pending, so the
// pass fails with a pointer to where the reason is.
func waitAgent(ctx context.Context, eng Engine, baseline []string) error {
	cctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()
	var lastErr error
	for first := true; ; first = false {
		p, err := eng.ServiceProgress(cctx, NodeServiceName, baseline)
		switch {
		case err != nil:
			lastErr = err
		case p.Found && !first && strings.HasPrefix(p.UpdateState, "rollback"):
			return errors.New("agent update rolled back: Swarm restored the previous agent spec")
		case p.Found && p.Desired > 0 && p.Running >= p.Desired && docker.UpdateSettled(p.UpdateState):
			return nil
		}
		select {
		case <-cctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("the agent did not start within %s: %w", convergeTimeout, lastErr)
			}
			return fmt.Errorf("the agent is not running on every node after %s (see docker service ps %s)", convergeTimeout, NodeServiceName)
		case <-time.After(convergePoll):
		}
	}
}
