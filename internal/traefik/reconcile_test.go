package traefik

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// fakeEngine records what Reconcile asks of Docker and plays back the labels of
// the service it last deployed, so a second Reconcile sees a running gateway.
// Every other Engine method panics (embedded nil interface) — Reconcile must
// not reach for anything else.
//
// It also models the gateway's tasks the way Swarm reports them: each deploy
// adds a task, which runs once pollsToRun progress polls have passed — or never,
// when it was deployed while stuck was set, the way a start-first replacement
// stays Pending behind a task holding a host port until the next deploy
// replaces it.
type fakeEngine struct {
	docker.Engine
	ensured  []string
	deployed []docker.ServiceSpec
	labels   map[string]string
	found    bool
	labelErr error
	creates  map[string]bool // network name -> NetworkEnsure reports it was created

	running      string // id of the task currently running; "" before the first deploy
	pending      string // id of a deployed task that has not started yet
	updateState  string
	stuck        bool // tasks deployed from now on never start
	pendingStuck bool // the pending task was deployed while stuck was set
	rollback     bool // Swarm rolls a deployed task back instead of running it
	pollsToRun   int  // polls a deployed task stays pending before it runs
	// polls Swarm keeps the update "updating" after the task runs — its
	// monitor period, during which a failing task would still be rolled back
	pollsToComplete int
	polls           int
	sinceRun        int
	baselines       [][]string // the exclude list of every progress poll
	progressErr     error
}

func (f *fakeEngine) NetworkEnsure(_ context.Context, name string) (bool, error) {
	f.ensured = append(f.ensured, name)
	return f.creates[name], nil
}

func (f *fakeEngine) ServiceDeploy(_ context.Context, spec docker.ServiceSpec) error {
	f.deployed = append(f.deployed, spec)
	f.labels = spec.Labels
	f.found = true
	f.pending = fmt.Sprintf("task-%d", len(f.deployed))
	f.updateState = "updating"
	f.pendingStuck = f.stuck
	f.polls = 0
	return nil
}

func (f *fakeEngine) ServiceProgress(_ context.Context, _ string, exclude []string) (docker.ServiceProgress, error) {
	f.baselines = append(f.baselines, exclude)
	if f.progressErr != nil {
		return docker.ServiceProgress{}, f.progressErr
	}
	if !f.found {
		return docker.ServiceProgress{}, nil
	}
	switch {
	case f.pending != "" && !f.pendingStuck:
		f.polls++
		switch {
		case f.rollback && f.polls > 1:
			f.pending, f.updateState = "", "rollback_completed"
		case !f.rollback && f.polls > f.pollsToRun:
			f.running, f.pending, f.sinceRun = f.pending, "", 0
			if f.pollsToComplete == 0 {
				f.updateState = "completed"
			}
		}
	case f.pending == "" && f.running != "" && f.updateState == "updating":
		f.sinceRun++
		if f.sinceRun >= f.pollsToComplete {
			f.updateState = "completed"
		}
	}
	p := docker.ServiceProgress{Found: true, Desired: 1, UpdateState: f.updateState}
	if f.running != "" {
		p.TaskIDs = []string{f.running}
		if !slices.Contains(exclude, f.running) {
			p.Running = 1
		}
	}
	return p, nil
}

// fastConvergence shortens the wait for the gateway's new task for one test.
func fastConvergence(t *testing.T, timeout time.Duration) {
	t.Helper()
	prevTimeout, prevPoll := convergeTimeout, convergePoll
	convergeTimeout, convergePoll = timeout, time.Millisecond
	t.Cleanup(func() { convergeTimeout, convergePoll = prevTimeout, prevPoll })
}

func (f *fakeEngine) ServiceLabels(_ context.Context, _ string) (map[string]string, bool, error) {
	if f.labelErr != nil {
		return nil, false, f.labelErr
	}
	return f.labels, f.found, nil
}

func TestReconcileAttachesBaseAndOrgNetworks(t *testing.T) {
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, acme, PanelProvider{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := []string{"krill-net", "krill-org-1", "krill-org-2"}
	if len(f.ensured) != len(want) {
		t.Fatalf("ensured = %v, want %v", f.ensured, want)
	}
	for i, n := range want {
		if f.ensured[i] != n {
			t.Fatalf("ensured = %v, want %v", f.ensured, want)
		}
	}
	if len(f.deployed) != 1 {
		t.Fatalf("want one deploy, got %d", len(f.deployed))
	}
	got := f.deployed[0].Networks
	if len(got) != len(want) {
		t.Fatalf("spec networks = %v, want %v", got, want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Fatalf("spec networks = %v, want %v (base first, stable order)", got, want)
		}
	}
	// The router default network is the base one: a router with no explicit
	// network label must still resolve.
	if !hasArg(f.deployed[0].Args, "--providers.swarm.network=krill-net") {
		t.Fatalf("swarm provider network arg missing: %v", f.deployed[0].Args)
	}
}

// Swarm recreates the Traefik task on every ServiceDeploy (ForceUpdate is
// bumped unconditionally), so a reconcile that changes nothing must not deploy
// at all — otherwise every control-plane restart and every unrelated call would
// blip ingress.
func TestReconcileIsIdempotent(t *testing.T) {
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	orgs := []string{"krill-org-1"}
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme, PanelProvider{}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme, PanelProvider{}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("unchanged reconcile redeployed: %d deploys", len(f.deployed))
	}

	// A new organization changes the network set, so the gateway has to move.
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, acme, PanelProvider{}); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if len(f.deployed) != 2 {
		t.Fatalf("a changed network set must redeploy: %d deploys", len(f.deployed))
	}

	// So does a changed Let's Encrypt configuration: the fingerprint covers the
	// whole spec, not only the networks.
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, AcmeConfig{Email: "a@b.c", Staging: true}, PanelProvider{}); err != nil {
		t.Fatalf("fourth reconcile: %v", err)
	}
	if len(f.deployed) != 3 {
		t.Fatalf("a changed acme config must redeploy: %d deploys", len(f.deployed))
	}
}

// If the current labels cannot be read we cannot tell whether anything changed;
// deploying is the safe answer (a redundant restart beats a gateway that never
// learns about a new network).
func TestReconcileDeploysWhenLabelsUnreadable(t *testing.T) {
	f := &fakeEngine{labelErr: errors.New("boom")}
	if err := Reconcile(context.Background(), f, "krill-net", nil, AcmeConfig{}, PanelProvider{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("want a deploy despite the read failure, got %d", len(f.deployed))
	}
}

// A network that was deleted and recreated by NetworkEnsure has a NEW id, while
// the gateway's stored spec still holds the old one. The name set — and so the
// fingerprint — is unchanged, so the skip has to be overridden by the fact that
// a network was created, or the gateway stays attached to a network that no
// longer exists.
func TestReconcileDeploysWhenANetworkWasRecreated(t *testing.T) {
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	orgs := []string{"krill-org-1"}
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme, PanelProvider{}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("want one deploy, got %d", len(f.deployed))
	}
	// Unchanged set, nothing created: still a no-op.
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme, PanelProvider{}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("unchanged reconcile redeployed: %d deploys", len(f.deployed))
	}
	// Same set, but one network had to be recreated.
	f.creates = map[string]bool{"krill-org-1": true}
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme, PanelProvider{}); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if len(f.deployed) != 2 {
		t.Fatalf("a recreated network must redeploy: %d deploys", len(f.deployed))
	}
}

// A deploy only hands Swarm a new spec; the gateway has moved when its new task
// runs. Reconcile waits for that task — counted against the tasks that were
// running before the deploy, so the old task cannot pass for the new one.
func TestReconcileWaitsForTheNewTask(t *testing.T) {
	fastConvergence(t, time.Second)
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1"}, acme, PanelProvider{}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	f.pollsToRun = 3
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, acme, PanelProvider{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if f.running != "task-2" {
		t.Fatalf("reconcile returned before the new task ran (running %q)", f.running)
	}
	last := f.baselines[len(f.baselines)-1]
	if !slices.Contains(last, "task-1") {
		t.Fatalf("progress was not measured against the pre-deploy tasks: exclude = %v", last)
	}
}

// The failure this guards against: a replacement that never starts leaves the
// old task serving the old networks. Reconcile has to report it, or the caller
// moves services into networks the gateway never joined.
func TestReconcileFailsWhenTheNewTaskNeverRuns(t *testing.T) {
	fastConvergence(t, 50*time.Millisecond)
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1"}, acme, PanelProvider{}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	f.stuck = true
	err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, acme, PanelProvider{})
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("err = %v, want a gateway that did not start", err)
	}
}

func TestReconcileFailsWhenSwarmRollsBack(t *testing.T) {
	fastConvergence(t, time.Second)
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	if err := Reconcile(context.Background(), f, "krill-net", nil, acme, PanelProvider{}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	f.rollback = true
	err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1"}, acme, PanelProvider{})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v, want a rolled-back update", err)
	}
}

// An update left stuck by an earlier deploy carries the current fingerprint
// label even though its task never started — the label is written with the
// spec, not when the task runs. Reconcile must not take that label as proof the
// gateway is up to date, or an install stuck before this fix stays stuck.
func TestReconcileRedeploysAnUpdateThatNeverFinished(t *testing.T) {
	fastConvergence(t, 50*time.Millisecond)
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	orgs := []string{"krill-org-1"}
	if err := Reconcile(context.Background(), f, "krill-net", nil, acme, PanelProvider{}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	f.stuck = true
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme, PanelProvider{}); err == nil {
		t.Fatal("a stuck update must fail the reconcile")
	}
	f.stuck = false
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme, PanelProvider{}); err != nil {
		t.Fatalf("reconcile after the stuck update: %v", err)
	}
	if len(f.deployed) != 3 {
		t.Fatalf("an unfinished update with a matching label was skipped: %d deploys", len(f.deployed))
	}
	if f.running != "task-3" {
		t.Fatalf("the redeployed task is not running: %q", f.running)
	}
}

// Without a baseline the new task cannot be told from the old one, so a
// progress read that fails is an error rather than an unverified success.
func TestReconcileFailsWhenProgressIsUnreadable(t *testing.T) {
	fastConvergence(t, time.Second)
	f := &fakeEngine{progressErr: errors.New("boom")}
	if err := Reconcile(context.Background(), f, "krill-net", nil, AcmeConfig{}, PanelProvider{}); err == nil {
		t.Fatal("want an error when the gateway's tasks cannot be read")
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed without a baseline: %d deploys", len(f.deployed))
	}
}

// Swarm reports an update "updating" for its monitor period after the new task
// starts, and rolls it back if the task fails within it. Reconcile returns only
// once the update is complete: a gateway still inside that window is neither
// settled nor safe to build on, and a second Reconcile would redeploy it.
func TestReconcileWaitsForSwarmToCompleteTheUpdate(t *testing.T) {
	fastConvergence(t, time.Second)
	f := &fakeEngine{pollsToComplete: 3}
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1"}, AcmeConfig{}, PanelProvider{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if f.updateState != "completed" {
		t.Fatalf("reconcile returned while the update was %q", f.updateState)
	}
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1"}, AcmeConfig{}, PanelProvider{}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("an unchanged gateway was redeployed: %d deploys", len(f.deployed))
	}
}
