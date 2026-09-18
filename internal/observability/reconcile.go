package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	// ServiceTasks reports every current task and the node it runs on — used
	// after a pass converges to tell a node still running its PREVIOUS task
	// from one running the new one (see Coverage).
	ServiceTasks(ctx context.Context, name string) ([]docker.TaskPlacement, error)
	docker.SwarmObjects
}

// Coverage is how many of the nodes Reconcile still expects a container on
// (see NodeName.Expected) actually run its current configuration, once the
// pass has confirmed the service itself converged. Partial coverage is not a
// failure — the pass still succeeds — but an operator who just rotated a
// push credential needs to know a node is still sending with the PREVIOUS
// settings, not merely that it is unreachable.
type Coverage struct {
	Expected int
	Deployed int
	Missing  []string // Krill display names of expected nodes still on the previous configuration
}

// expectedNodes is the subset of nodes Reconcile still expects a container on
// this pass: NodeName.Expected (Availability != "drain"). A drained node has
// had its task actively removed by Swarm, so its absence is not news; a down
// or paused node has not — that is exactly the case Missing exists to name.
func expectedNodes(nodes []NodeName) []NodeName {
	var expected []NodeName
	for _, n := range nodes {
		if n.Expected {
			expected = append(expected, n)
		}
	}
	return expected
}

// anyHostnameKnown reports whether any task's NodeName matches one of nodes'
// hostnames. docker.Engine.ServiceTasks falls back to raw Swarm node IDs
// when it cannot resolve nodes to hostnames (a degraded/partial NodeList);
// when that happens every task's "hostname" is a string that will never
// match, and coverage must tell that apart from a cluster that is genuinely
// all missing — see its use in coverage.
func anyHostnameKnown(nodes []NodeName, tasks []docker.TaskPlacement) bool {
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.Hostname != "" {
			known[n.Hostname] = true
		}
	}
	for _, t := range tasks {
		if known[t.NodeName] {
			return true
		}
	}
	return false
}

// coverage reports, for each expected node (see expectedNodes), whether
// ServiceTasks currently shows it running a task that is not in stale and is
// itself running. stale is the pre-deploy baseline task IDs this call's
// caller just rolled the spec forward from: a node whose only task is still
// in stale never received the update — its container just never got a new
// assignment — even though its State still reports "running" (see
// docker.TaskPlacement).
//
// Two conditions degrade to the zero Coverage rather than a real answer:
// ServiceTasks erroring (the agent IS deployed; a transient listing failure
// must not turn a successful pass into a false alarm), and ServiceTasks
// returning tasks whose NodeName resolves to none of nodes' hostnames at all
// (see anyHostnameKnown) — without real per-node data, calling every
// expected node "missing" would be a louder false alarm than saying nothing.
func coverage(ctx context.Context, eng Engine, nodes []NodeName, stale []string) Coverage {
	expected := expectedNodes(nodes)
	if len(expected) == 0 {
		return Coverage{}
	}
	tasks, err := eng.ServiceTasks(ctx, NodeServiceName)
	if err != nil {
		slog.Warn("observability: could not check node coverage", "err", err)
		return Coverage{}
	}
	if len(tasks) > 0 && !anyHostnameKnown(nodes, tasks) {
		slog.Warn("observability: ServiceTasks did not resolve any node hostname; skipping node coverage for this pass")
		return Coverage{}
	}
	old := make(map[string]bool, len(stale))
	for _, id := range stale {
		old[id] = true
	}
	// Keyed by hostname: two nodes sharing a hostname (not possible on a real
	// Swarm cluster, where it's the node identity) would collapse into one
	// entry here — an accepted, unlikely edge case, not a bug to guard
	// against.
	up := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if t.State == "running" && !old[t.ID] {
			up[t.NodeName] = true
		}
	}
	cov := Coverage{Expected: len(expected)}
	for _, n := range expected {
		if up[n.Hostname] {
			cov.Deployed++
		} else {
			cov.Missing = append(cov.Missing, n.DisplayName())
		}
	}
	// Missing is built in the order expected() (and so nodes, and so
	// engine.Nodes()) happens to return, which Swarm does not guarantee —
	// without this, the names in the UI line could reorder between two
	// passes that found the exact same nodes missing.
	sort.Strings(cov.Missing)
	return cov
}

// convergeTimeout covers pulling the agent image on every node.
var (
	convergeTimeout = 5 * time.Minute
	convergePoll    = 2 * time.Second
)

// Reconcile converges the agent to s: deployed with this configuration when
// enabled, absent otherwise. nodes maps Swarm hostnames to the names Krill
// shows for them (see RenderNodeConfig) and marks which ones Reconcile still
// expects a container on (see NodeName.Expected); both are unused when s is
// disabled. The returned Coverage is only meaningful on a nil error, and is
// only ever non-zero right after an actual deploy-and-converge on this pass
// — the short-circuit "nothing changed" path always returns the zero
// Coverage (see its own comment for why).
func Reconcile(ctx context.Context, eng Engine, s Settings, network string, nodes []NodeName) (Coverage, error) {
	if !s.Enabled {
		return Coverage{}, teardown(ctx, eng)
	}
	cfg, err := RenderNodeConfig(s, nodes)
	if err != nil {
		return Coverage{}, err
	}
	obj := objectsOf(s, cfg)
	labels := map[string]string{ObjectLabel: ObjectLabelValue}
	if err := eng.ConfigEnsure(ctx, obj.Config, cfg, labels); err != nil {
		return Coverage{}, fmt.Errorf("create the agent configuration: %w", err)
	}
	if obj.MetricsSecret != "" {
		if err := eng.SecretEnsure(ctx, obj.MetricsSecret, []byte(s.Metrics.Password), labels); err != nil {
			return Coverage{}, fmt.Errorf("create the metrics password secret: %w", err)
		}
	}
	if obj.LogsSecret != "" {
		if err := eng.SecretEnsure(ctx, obj.LogsSecret, []byte(s.Logs.Password), labels); err != nil {
			return Coverage{}, fmt.Errorf("create the logs password secret: %w", err)
		}
	}
	netCreated, err := eng.NetworkEnsure(ctx, network)
	if err != nil {
		return Coverage{}, err
	}
	spec := NodeSpec(obj, network)
	before, err := eng.ServiceProgress(ctx, NodeServiceName, nil)
	if err != nil {
		return Coverage{}, fmt.Errorf("read the agent's tasks: %w", err)
	}
	// A matching label proves the spec was written, not that it finished
	// rolling out; an unsettled update is deployed again. A network that was
	// just (re)created has the same name but is new to Swarm, so the service
	// is deployed again to attach to it.
	if before.Found && !netCreated && docker.UpdateSettled(before.UpdateState) {
		if cur, found, err := eng.ServiceLabels(ctx, NodeServiceName); err == nil && found {
			if h := spec.Labels[specHashLabel]; h != "" && cur[specHashLabel] == h {
				prune(ctx, eng, obj)
				// Nothing was redeployed on this pass, so there is no
				// pre-deploy baseline to tell "still running this exact
				// configuration" apart from "not running at all" (a
				// restart, an image pull, a crash loop): reporting
				// coverage here would risk calling a node that isn't
				// running ANYTHING "still on the previous settings". The
				// existing "Agents running: X of Y" line already covers
				// the not-running case, so this path reports nothing.
				return Coverage{}, nil
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
		return Coverage{}, fmt.Errorf("deploy the agent: %w", err)
	}
	if err := waitAgent(ctx, eng, baseline); err != nil {
		return Coverage{}, err
	}
	slog.Info("observability agent deployed")
	prune(ctx, eng, obj)
	return coverage(ctx, eng, nodes, baseline), nil
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
