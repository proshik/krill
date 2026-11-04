package deploy

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

type routingBaselineEngine struct {
	*mockEngine
	task    string
	applied bool
}

func (e *routingBaselineEngine) ServiceDeploy(ctx context.Context, s docker.ServiceSpec) error {
	e.applied = true
	return e.mockEngine.ServiceDeploy(ctx, s)
}
func (e *routingBaselineEngine) ServiceProgress(_ context.Context, _ string, baseline []string) (docker.ServiceProgress, error) {
	state := docker.ServiceProgress{Found: true, Desired: 1, TaskIDs: []string{e.task}, Running: 1}
	if e.applied {
		// The replacement crashes; the only running task is a restart of the OLD
		// image that appeared while the deploy waited for current routing state.
		state.Failed = 3
		for _, id := range baseline {
			if id == e.task {
				state.Running = 0
			}
		}
	}
	return state, nil
}

type routingBaselineStore struct {
	*fakeStore
	refresh func()
}

func (s *routingBaselineStore) RefreshRouting(context.Context, *App) error { s.refresh(); return nil }

func TestDeployBaselineIncludesRestartBeforeFinalApply(t *testing.T) {
	e := &routingBaselineEngine{mockEngine: &mockEngine{}, task: "old-task"}
	st := &routingBaselineStore{fakeStore: newFakeStore(imageApp()), refresh: func() { e.task = "restarted-old-image" }}
	d := newDeployer(e, &mockBuilder{}, st)
	id := d.Enqueue(1, TriggerManual)
	d.run(t.Context(), <-d.queue)
	if st.depStatus(id) != "error" {
		t.Fatalf("old-image restart counted as new release: %s", st.depStatus(id))
	}
	if !e.applied {
		t.Fatal("test did not apply replacement spec")
	}
}
