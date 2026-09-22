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
func reconcileNode(ctx context.Context, eng Engine, s Settings, network string, nodes []NodeName) (Coverage, error) {
	if !s.Enabled {
		return Coverage{}, teardownService(ctx, eng, NodeServiceName, ObjectLabelValue)
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
				pruneObjects(ctx, eng, ObjectLabelValue, []string{obj.Config, obj.MetricsSecret, obj.LogsSecret})
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
	if err := waitService(ctx, eng, NodeServiceName, baseline); err != nil {
		return Coverage{}, err
	}
	slog.Info("observability agent deployed")
	pruneObjects(ctx, eng, ObjectLabelValue, []string{obj.Config, obj.MetricsSecret, obj.LogsSecret})
	return coverage(ctx, eng, nodes, baseline), nil
}

func teardownService(ctx context.Context, eng Engine, name, labelValue string) error {
	_, found, err := eng.ServiceLabels(ctx, name)
	if err != nil {
		return fmt.Errorf("read %s service: %w", name, err)
	}
	if found {
		if err := eng.ServiceRemove(ctx, name); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
		slog.Info("observability service removed", "service", name)
	}
	pruneObjects(ctx, eng, labelValue, nil)
	return nil
}

// pruneObjects removes only obsolete objects owned by one collector.
func pruneObjects(ctx context.Context, eng Engine, labelValue string, names []string) {
	var keep []string
	for _, name := range names {
		if name != "" {
			keep = append(keep, name)
		}
	}
	if err := eng.PruneObjects(ctx, ObjectLabel, labelValue, keep); err != nil {
		slog.Warn("observability: could not remove old configs and secrets", "collector", labelValue, "err", err)
	}
}

// waitService polls until the service's new tasks run and Swarm has
// completed the update. A node that is down keeps its task pending, so the
// pass fails with a pointer to where the reason is.
func waitService(ctx context.Context, eng Engine, name string, baseline []string) error {
	cctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()
	var lastErr error
	for first := true; ; first = false {
		p, err := eng.ServiceProgress(cctx, name, baseline)
		switch {
		case err != nil:
			lastErr = err
		case p.Found && !first && strings.HasPrefix(p.UpdateState, "rollback"):
			return fmt.Errorf("%s update rolled back: Swarm restored the previous spec", name)
		case p.Found && p.Desired > 0 && p.Running >= p.Desired && docker.UpdateSettled(p.UpdateState):
			return nil
		}
		select {
		case <-cctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("%s did not start within %s: %w", name, convergeTimeout, lastErr)
			}
			return fmt.Errorf("%s is not running everywhere it should after %s (see docker service ps %s)", name, convergeTimeout, name)
		case <-time.After(convergePoll):
		}
	}
}

func Reconcile(ctx context.Context, eng Engine, s Settings, network string, nodes []NodeName, apps AppsInput) (Coverage, error) {
	if !s.Enabled {
		return Coverage{}, errors.Join(
			teardownService(ctx, eng, NodeServiceName, ObjectLabelValue),
			teardownService(ctx, eng, AppsServiceName, ObjectLabelAppsValue))
	}
	cov, nodeErr := reconcileNode(ctx, eng, s, network, nodes)
	appsErr := reconcileApps(ctx, eng, s, network, apps)
	return cov, errors.Join(nodeErr, appsErr)
}

// reconcileApps converges the apps collector: present while metrics have a
// target (it ships nothing else), absent otherwise. When Krill cannot be
// reached by it (no advertise address), whatever runs is left alone — tearing
// a working collector down because the control plane's own config regressed
// would silence every app — and the pass reports why.
func reconcileApps(ctx context.Context, eng Engine, s Settings, network string, apps AppsInput) error {
	if !s.Metrics.Configured() {
		return teardownService(ctx, eng, AppsServiceName, ObjectLabelAppsValue)
	}
	if apps.Unavailable != nil {
		return fmt.Errorf("the apps collector cannot reach Krill: %w", apps.Unavailable)
	}
	if apps.ProviderToken == "" || !safeText(apps.ProviderToken) {
		return errors.New("the apps collector provider credential is unavailable")
	}
	cfg, err := RenderAppsConfig(s, apps.ProviderURL)
	if err != nil {
		return err
	}
	obj := appsObjectsOf(s, cfg, apps.ProviderToken)
	labels := map[string]string{ObjectLabel: ObjectLabelAppsValue}
	if err := eng.ConfigEnsure(ctx, obj.Config, cfg, labels); err != nil {
		return fmt.Errorf("create the collector configuration: %w", err)
	}
	if err := eng.SecretEnsure(ctx, obj.ProviderSecret, []byte(apps.ProviderToken), labels); err != nil {
		return fmt.Errorf("create the collector provider secret: %w", err)
	}
	if obj.MetricsSecret != "" {
		if err := eng.SecretEnsure(ctx, obj.MetricsSecret, []byte(s.Metrics.Password), labels); err != nil {
			return fmt.Errorf("create the collector metrics password secret: %w", err)
		}
	}
	created := false
	for _, n := range append([]string{network}, apps.Networks...) {
		c, err := eng.NetworkEnsure(ctx, n)
		if err != nil {
			return err
		}
		created = created || c
	}
	spec := AppsSpec(obj, network, apps.Networks)
	keep := []string{obj.Config, obj.ProviderSecret, obj.MetricsSecret}
	before, err := eng.ServiceProgress(ctx, AppsServiceName, nil)
	if err != nil {
		return fmt.Errorf("read the collector's tasks: %w", err)
	}
	if before.Found && !created && docker.UpdateSettled(before.UpdateState) {
		if cur, found, err := eng.ServiceLabels(ctx, AppsServiceName); err == nil && found &&
			spec.Labels[specHashLabel] != "" && cur[specHashLabel] == spec.Labels[specHashLabel] {
			pruneObjects(ctx, eng, ObjectLabelAppsValue, keep)
			return nil
		}
	}
	var baseline []string
	if before.Found {
		baseline = before.TaskIDs
	}
	slog.Info("deploying the apps metrics collector", "networks", len(apps.Networks)+1)
	if err := eng.ServiceDeploy(ctx, spec); err != nil {
		return fmt.Errorf("deploy the collector: %w", err)
	}
	if err := waitService(ctx, eng, AppsServiceName, baseline); err != nil {
		return err
	}
	slog.Info("apps metrics collector deployed")
	pruneObjects(ctx, eng, ObjectLabelAppsValue, keep)
	return nil
}
