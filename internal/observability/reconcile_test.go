package observability

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

type fakeEngine struct {
	mu       sync.Mutex
	objects  map[string][]byte // name -> data (configs and secrets together)
	pruned   [][]string        // keep lists passed to PruneObjects
	deploys  []docker.ServiceSpec
	removed  int
	labels   map[string]string // labels of the running service; nil = absent
	update   string            // UpdateState reported before a deploy
	rollback bool              // a deploy rolls back
	neverRun bool              // a deploy's task never runs
	created  bool              // NetworkEnsure reports the network as new
}

func newFakeEngine() *fakeEngine { return &fakeEngine{objects: map[string][]byte{}} }

func (f *fakeEngine) NetworkEnsure(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, nil
}
func (f *fakeEngine) ServiceDeploy(_ context.Context, s docker.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploys = append(f.deploys, s)
	f.labels = s.Labels
	f.update = "deployed"
	return nil
}
func (f *fakeEngine) ServiceRemove(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed++
	f.labels = nil
	return nil
}
func (f *fakeEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return docker.ServiceState{Found: f.labels != nil, Running: 1, Desired: 1}, nil
}
func (f *fakeEngine) ServiceProgress(_ context.Context, _ string, exclude []string) (docker.ServiceProgress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labels == nil {
		return docker.ServiceProgress{}, nil
	}
	if f.update != "deployed" { // before any deploy in this test
		return docker.ServiceProgress{Found: true, Desired: 1, Running: 1, UpdateState: f.update, TaskIDs: []string{"old"}}, nil
	}
	switch {
	case f.rollback:
		return docker.ServiceProgress{Found: true, Desired: 1, UpdateState: "rollback_completed"}, nil
	case f.neverRun:
		return docker.ServiceProgress{Found: true, Desired: 1, UpdateState: "updating"}, nil
	}
	return docker.ServiceProgress{Found: true, Desired: 1, Running: 1, UpdateState: "completed", TaskIDs: []string{"new"}}, nil
}
func (f *fakeEngine) ServiceLabels(context.Context, string) (map[string]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.labels, f.labels != nil, nil
}
func (f *fakeEngine) ConfigEnsure(_ context.Context, name string, data []byte, labels map[string]string) error {
	return f.ensure(name, data, labels)
}
func (f *fakeEngine) SecretEnsure(_ context.Context, name string, data []byte, labels map[string]string) error {
	return f.ensure(name, data, labels)
}
func (f *fakeEngine) ensure(name string, data []byte, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if labels[ObjectLabel] != ObjectLabelValue {
		return errors.New("object created without the ownership label")
	}
	f.objects[name] = data
	return nil
}
func (f *fakeEngine) PruneObjects(_ context.Context, key, value string, keep []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key != ObjectLabel || value != ObjectLabelValue {
		return errors.New("prune with a foreign label")
	}
	f.pruned = append(f.pruned, keep)
	return nil
}

func fastConverge(t *testing.T) {
	t.Helper()
	oldT, oldP := convergeTimeout, convergePoll
	convergeTimeout, convergePoll = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { convergeTimeout, convergePoll = oldT, oldP })
}

func TestReconcileDeploysAgent(t *testing.T) {
	fastConverge(t)
	eng := newFakeEngine()
	if err := Reconcile(context.Background(), eng, fullSettings(), "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if len(eng.deploys) != 1 {
		t.Fatalf("deploys = %d", len(eng.deploys))
	}
	s := eng.deploys[0]
	if s.Name != NodeServiceName || s.Image != Image || !s.Global || s.Network != "krill-net" ||
		s.Hostname != "{{.Node.Hostname}}" || !s.UpdateStopFirst ||
		s.MemoryLimitBytes != NodeMemoryLimit || s.NanoCPUs != NodeNanoCPUs || len(s.Ports) != 0 {
		t.Errorf("spec = %+v", s)
	}
	if strings.Join(s.Args, " ") != "run --server.http.listen-addr=127.0.0.1:12345 --storage.path=/var/lib/alloy/data /etc/alloy/config.alloy" {
		t.Errorf("args = %v", s.Args)
	}
	mounts := map[string]docker.MountSpec{}
	for _, m := range s.Mounts {
		mounts[m.Target] = m
	}
	for _, target := range []string{"/host/proc", "/host/sys", "/host/root", "/var/run/docker.sock"} {
		if m, ok := mounts[target]; !ok || !m.ReadOnly || m.Type == "volume" {
			t.Errorf("mount %s = %+v", target, m)
		}
	}
	if m := mounts["/var/lib/alloy/data"]; m.Type != "volume" || m.Source != "krill-alloy-node-data" || m.ReadOnly {
		t.Errorf("data volume = %+v", m)
	}
	if len(s.Configs) != 1 || s.Configs[0].Target != configPath || !strings.HasPrefix(s.Configs[0].Name, "krill-alloy-node-") {
		t.Errorf("configs = %+v", s.Configs)
	}
	if len(s.Secrets) != 2 {
		t.Fatalf("secrets = %+v", s.Secrets)
	}
	for _, sec := range s.Secrets {
		if sec.Mode != 0o400 || (sec.Target != metricsSecretFile && sec.Target != logsSecretFile) {
			t.Errorf("secret ref = %+v", sec)
		}
		if data := string(eng.objects[sec.Name]); data != "mpw" && data != "lpw" {
			t.Errorf("secret %s holds %q", sec.Name, data)
		}
	}
	if strings.Contains(strings.Join(s.Args, " "), "mpw") || len(s.Env) != 0 {
		t.Error("a password leaked into the service spec")
	}
	if s.Labels[specHashLabel] == "" {
		t.Error("spec hash label missing")
	}
	if len(eng.pruned) != 1 || len(eng.pruned[0]) != 3 {
		t.Errorf("prune keep lists = %v", eng.pruned)
	}
}

func TestReconcileSkipsUnchangedAndRedeploysChanged(t *testing.T) {
	fastConverge(t)
	eng := newFakeEngine()
	ctx := context.Background()
	s := fullSettings()
	if err := Reconcile(ctx, eng, s, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(ctx, eng, s, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if len(eng.deploys) != 1 {
		t.Fatalf("unchanged settings redeployed: %d deploys", len(eng.deploys))
	}
	first := eng.deploys[0]
	s.Logs.Password = "rotated"
	if err := Reconcile(ctx, eng, s, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if len(eng.deploys) != 2 {
		t.Fatalf("password change not deployed: %d deploys", len(eng.deploys))
	}
	if eng.deploys[1].Labels[specHashLabel] == first.Labels[specHashLabel] {
		t.Error("spec hash did not change with the secret")
	}
}

func TestReconcileRedeploysWhileUpdateUnsettled(t *testing.T) {
	fastConverge(t)
	eng := newFakeEngine()
	s := fullSettings()
	eng.labels = NodeSpec(objectsFor(t, s, nil), "krill-net").Labels // same hash, but…
	eng.update = "paused"                                            // …the update never finished
	if err := Reconcile(context.Background(), eng, s, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if len(eng.deploys) != 1 {
		t.Errorf("deploys = %d, want 1", len(eng.deploys))
	}
}

// A recreated network is new to Swarm even under the old name, so a service
// whose spec hash still matches must be deployed again to attach to it.
func TestReconcileRedeploysWhenNetworkRecreated(t *testing.T) {
	fastConverge(t)
	eng := newFakeEngine()
	ctx := context.Background()
	s := fullSettings()
	if err := Reconcile(ctx, eng, s, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.created = true
	eng.mu.Unlock()
	if err := Reconcile(ctx, eng, s, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if len(eng.deploys) != 2 {
		t.Fatalf("deploys = %d, want 2 (the network was recreated)", len(eng.deploys))
	}
	if eng.deploys[1].Labels[specHashLabel] != eng.deploys[0].Labels[specHashLabel] {
		t.Error("the spec itself changed; the test no longer isolates the network flag")
	}
}

func TestReconcileFailures(t *testing.T) {
	fastConverge(t)
	ctx := context.Background()

	eng := newFakeEngine()
	eng.rollback = true
	if err := Reconcile(ctx, eng, fullSettings(), "krill-net", nil); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("rollback: %v", err)
	}
	if len(eng.pruned) != 0 {
		t.Error("objects pruned after a rolled-back deploy")
	}

	eng = newFakeEngine()
	eng.neverRun = true
	err := Reconcile(ctx, eng, fullSettings(), "krill-net", nil)
	if err == nil || !strings.Contains(err.Error(), "docker service ps "+NodeServiceName) {
		t.Errorf("timeout: %v", err)
	}
	if len(eng.pruned) != 0 {
		t.Error("objects pruned after a failed deploy")
	}

	eng = newFakeEngine()
	if err := Reconcile(ctx, eng, Settings{Enabled: true}, "krill-net", nil); !errors.Is(err, ErrNothingConfigured) || len(eng.deploys) != 0 {
		t.Errorf("nothing configured: %v, deploys %d", err, len(eng.deploys))
	}
}

func TestReconcileDisabled(t *testing.T) {
	ctx := context.Background()
	eng := newFakeEngine()
	if err := Reconcile(ctx, eng, Settings{}, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if eng.removed != 0 || len(eng.pruned) != 1 || len(eng.pruned[0]) != 0 {
		t.Errorf("absent service: removed=%d pruned=%v", eng.removed, eng.pruned)
	}
	eng.labels = map[string]string{specHashLabel: "x"}
	s := fullSettings()
	s.Enabled = false
	if err := Reconcile(ctx, eng, s, "krill-net", nil); err != nil {
		t.Fatal(err)
	}
	if eng.removed != 1 {
		t.Errorf("running service not removed")
	}
}

// TestReconcileUserWithoutPasswordHasNoSecret is the extra case the plan
// calls for: a user name alone authenticates nothing, so it must not create
// a secret or reference one from the rendered configuration.
func TestReconcileUserWithoutPasswordHasNoSecret(t *testing.T) {
	s := Settings{Enabled: true, Metrics: Target{URL: "https://m.example.com/api/v1/push", User: "tenant"}}
	cfg, err := RenderNodeConfig(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "password_file") {
		t.Errorf("config for a user without a password references a secret file:\n%s", cfg)
	}
	obj := objectsOf(s, cfg)
	if obj.MetricsSecret != "" || obj.LogsSecret != "" {
		t.Errorf("objectsOf created a secret for a user without a password: %+v", obj)
	}
	spec := NodeSpec(obj, "krill-net")
	if len(spec.Secrets) != 0 {
		t.Errorf("NodeSpec added a secret for a user without a password: %+v", spec.Secrets)
	}
}

func objectsFor(t *testing.T, s Settings, nodes []NodeName) Objects {
	t.Helper()
	cfg, err := RenderNodeConfig(s, nodes)
	if err != nil {
		t.Fatal(err)
	}
	return objectsOf(s, cfg)
}

// TestReconcileNodeListChangeRedeploys proves the node name mapping is part of
// the content that names the config object: a node joining or leaving the
// cluster must redeploy the agent, exactly like a changed push address would.
func TestReconcileNodeListChangeRedeploys(t *testing.T) {
	fastConverge(t)
	eng := newFakeEngine()
	ctx := context.Background()
	s := fullSettings()
	if err := Reconcile(ctx, eng, s, "krill-net", []NodeName{{Hostname: "h1", Name: "control-plane"}}); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(ctx, eng, s, "krill-net", []NodeName{{Hostname: "h1", Name: "control-plane"}}); err != nil {
		t.Fatal(err)
	}
	if len(eng.deploys) != 1 {
		t.Fatalf("unchanged node list redeployed: %d deploys", len(eng.deploys))
	}
	first := eng.deploys[0]
	// A worker joins.
	nodes := []NodeName{{Hostname: "h1", Name: "control-plane"}, {Hostname: "h2", Name: "worker-1"}}
	if err := Reconcile(ctx, eng, s, "krill-net", nodes); err != nil {
		t.Fatal(err)
	}
	if len(eng.deploys) != 2 {
		t.Fatalf("node list change not deployed: %d deploys", len(eng.deploys))
	}
	if eng.deploys[1].Labels[specHashLabel] == first.Labels[specHashLabel] {
		t.Error("spec hash did not change with the node list")
	}
}
