package observability

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

func TestAppsConfigAndSpec(t *testing.T) {
	cfg, err := RenderAppsConfig(Settings{Enabled: true, Metrics: Target{URL: "https://m.example/api/v1/push", User: "u", Password: "p"}}, "http://10.0.0.1:8080/_krill/alloy/apps")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "apps_config.golden.alloy", cfg)
	if _, err := RenderAppsConfig(Settings{}, "http://x"); !errors.Is(err, ErrNothingConfigured) {
		t.Fatalf("empty metrics: %v", err)
	}
	if _, err := RenderAppsConfig(fullSettings(), `http://bad"url`); !errors.Is(err, errUnsafeText) {
		t.Fatalf("unsafe provider: %v", err)
	}
	obj := AppsObjects{Config: "c", MetricsSecret: "m", ProviderSecret: "p"}
	s := AppsSpec(obj, "krill-net", []string{"krill-org-2", "krill-org-1"})
	other := AppsSpec(obj, "krill-net", []string{"krill-org-1", "krill-org-2"})
	if !reflect.DeepEqual(s, other) {
		t.Fatal("network order affects spec")
	}
	if s.Name != AppsServiceName || s.Image != Image || s.Global || s.Replicas != 1 || !s.UpdateStopFirst || s.MemoryLimitBytes != 192<<20 || s.NanoCPUs != 250000000 || !reflect.DeepEqual(s.Constraints, []string{"node.role==manager"}) || !reflect.DeepEqual(s.Args, nodeArgs()) {
		t.Fatalf("collector spec: %+v", s)
	}
	if len(s.Mounts) != 1 || s.Mounts[0].Source != appsVolume || s.Mounts[0].Target != storagePath || len(s.Configs) != 1 || s.Configs[0].Target != configPath || len(s.Secrets) != 2 {
		t.Fatalf("mounts: %+v", s)
	}
	for _, secret := range s.Secrets {
		if secret.Mode != 0400 {
			t.Fatal("secret is not restricted")
		}
	}
	if s.Labels[specHashLabel] == "" {
		t.Fatal("no fingerprint")
	}
}

// dualEngine preserves the node-only fake's detailed rollout behavior while
// providing isolated state for each service and tracking object ownership.
type dualEngine struct {
	*fakeEngine
	apps     *fakeEngine
	owned    map[string]string
	failures map[string]error
}

func newDualEngine() *dualEngine {
	return &dualEngine{fakeEngine: newFakeEngine(), apps: newFakeEngine(), owned: map[string]string{}, failures: map[string]error{}}
}
func (f *dualEngine) service(name string) *fakeEngine {
	if name == AppsServiceName {
		return f.apps
	}
	return f.fakeEngine
}
func (f *dualEngine) ServiceDeploy(ctx context.Context, s docker.ServiceSpec) error {
	if err := f.failures[s.Name]; err != nil {
		return err
	}
	return f.service(s.Name).ServiceDeploy(ctx, s)
}
func (f *dualEngine) ServiceRemove(ctx context.Context, name string) error {
	return f.service(name).ServiceRemove(ctx, name)
}
func (f *dualEngine) ServiceLabels(ctx context.Context, name string) (map[string]string, bool, error) {
	return f.service(name).ServiceLabels(ctx, name)
}
func (f *dualEngine) ServiceProgress(ctx context.Context, name string, ids []string) (docker.ServiceProgress, error) {
	return f.service(name).ServiceProgress(ctx, name, ids)
}
func (f *dualEngine) ServiceState(ctx context.Context, name string) (docker.ServiceState, error) {
	return f.service(name).ServiceState(ctx, name)
}
func (f *dualEngine) ConfigEnsure(_ context.Context, name string, _ []byte, labels map[string]string) error {
	f.owned[name] = labels[ObjectLabel]
	return nil
}
func (f *dualEngine) SecretEnsure(ctx context.Context, name string, b []byte, labels map[string]string) error {
	return f.ConfigEnsure(ctx, name, b, labels)
}
func (f *dualEngine) PruneObjects(_ context.Context, key, value string, keep []string) error {
	if key != ObjectLabel {
		return errors.New("wrong ownership label")
	}
	for name, owner := range f.owned {
		if owner != value {
			continue
		}
		found := false
		for _, k := range keep {
			if k == name {
				found = true
			}
		}
		if !found {
			delete(f.owned, name)
		}
	}
	return nil
}
func validAppsInput() AppsInput {
	return AppsInput{ProviderURL: "http://10.0.0.1:8080/_krill/alloy/apps", ProviderToken: "provider", Networks: []string{"krill-org-1"}}
}
func TestAppsReconcileLifecycle(t *testing.T) {
	fastConverge(t)
	ctx := context.Background()
	f := newDualEngine()
	a := validAppsInput()
	if _, err := Reconcile(ctx, f, fullSettings(), "net", nil, a); err != nil {
		t.Fatal(err)
	}
	if len(f.deploys) != 1 || len(f.apps.deploys) != 1 {
		t.Fatal("both collectors must deploy")
	}
	counts := map[string]int{}
	for _, owner := range f.owned {
		counts[owner]++
	}
	if counts[ObjectLabelValue] != 3 || counts[ObjectLabelAppsValue] != 3 {
		t.Fatalf("objects lost or mislabeled: %v", counts)
	}
	if _, err := Reconcile(ctx, f, fullSettings(), "net", nil, a); err != nil {
		t.Fatal(err)
	}
	if len(f.apps.deploys) != 1 {
		t.Fatal("unchanged collector redeployed")
	}
	a.Networks = append(a.Networks, "krill-org-2")
	if _, err := Reconcile(ctx, f, fullSettings(), "net", nil, a); err != nil {
		t.Fatal(err)
	}
	if len(f.apps.deploys) != 2 {
		t.Fatal("new network not applied")
	}
	a.Unavailable = errors.New("KRILL_ADVERTISE_ADDR missing")
	if _, err := Reconcile(ctx, f, fullSettings(), "net", nil, a); err == nil || !strings.Contains(err.Error(), "KRILL_ADVERTISE_ADDR") {
		t.Fatalf("unavailable: %v", err)
	}
	if f.apps.labels == nil || len(f.apps.deploys) != 2 {
		t.Fatal("unavailable provider changed running collector")
	}
	logs := fullSettings()
	logs.Metrics = Target{}
	if _, err := Reconcile(ctx, f, logs, "net", nil, a); err != nil {
		t.Fatal(err)
	}
	if f.apps.labels != nil || f.labels == nil {
		t.Fatal("logs-only must remove apps collector alone")
	}
	if _, err := Reconcile(ctx, f, Settings{}, "net", nil, a); err != nil {
		t.Fatal(err)
	}
	if f.labels != nil || len(f.owned) != 0 {
		t.Fatal("disabled collectors or objects remain")
	}
}
func TestAppsReconcileIndependentFailures(t *testing.T) {
	fastConverge(t)
	f := newDualEngine()
	nodeErr := errors.New("node failure")
	appsErr := errors.New("apps failure")
	f.failures[NodeServiceName] = nodeErr
	_, err := Reconcile(context.Background(), f, fullSettings(), "net", nil, validAppsInput())
	if !errors.Is(err, nodeErr) || len(f.apps.deploys) != 1 {
		t.Fatalf("node failure blocked apps: %v", err)
	}
	f.apps.labels = nil
	f.failures[AppsServiceName] = appsErr
	_, err = Reconcile(context.Background(), f, fullSettings(), "net", nil, validAppsInput())
	if !errors.Is(err, nodeErr) || !errors.Is(err, appsErr) {
		t.Fatalf("errors not joined: %v", err)
	}
}
func TestAppsNetworkCooldown(t *testing.T) {
	a := &countingApply{done: make(chan struct{}, 10)}
	r := newTestReconciler(func(context.Context) (Settings, error) { return fullSettings(), nil }, a)
	r.debounce = time.Millisecond
	r.networksCooldown = 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.Trigger()
	waitDone(t, a.done)
	// waitDone can observe apply before pass records lastRun.
	deadline := time.Now().Add(time.Second)
	for r.Status(ctx).LastRun.IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.TriggerNetworks()
	r.TriggerNetworks()
	select {
	case <-a.done:
		t.Fatal("network cooldown ignored")
	case <-time.After(30 * time.Millisecond):
	}
	r.Trigger() // a settings change must interrupt the cooldown.
	select {
	case <-a.done:
	case <-time.After(80 * time.Millisecond):
		t.Fatal("settings held behind network cooldown")
	}
	r.TriggerNetworks()
	waitDone(t, a.done)
	select {
	case <-a.done:
		t.Fatal("duplicate network pass")
	case <-time.After(30 * time.Millisecond):
	}
}
