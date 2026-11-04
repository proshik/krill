package deploy

import (
	"context"
	"fmt"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

type placementReleaseEngine struct {
	*releaseEngine
	label            bool
	sets             int
	driftBeforeApply bool
}

func (e *placementReleaseEngine) Nodes(context.Context) ([]docker.SwarmNode, error) {
	return []docker.SwarmNode{{ID: "A"}}, nil
}
func (e *placementReleaseEngine) NodeSetLabel(context.Context, string, string, string) error {
	e.sets++
	e.label = true
	return nil
}
func (e *placementReleaseEngine) NodeDeleteLabel(context.Context, string, string) error {
	e.label = false
	return nil
}
func (e *placementReleaseEngine) ValidateRelease(context.Context, docker.ServiceSpec, []string) error {
	if !e.label {
		return fmt.Errorf("selected node no longer has app placement label")
	}
	return nil
}
func (e *placementReleaseEngine) ServiceProgress(ctx context.Context, name string, baseline []string) (docker.ServiceProgress, error) {
	if e.driftBeforeApply && baseline == nil {
		e.label = false
	}
	return e.releaseEngine.ServiceProgress(ctx, name, baseline)
}

type placementReleaseStore struct {
	*DBStore
	afterSave func()
}

func (s *placementReleaseStore) SaveRelease(ctx context.Context, appID, deployID int64, snap releaseSnapshot, source int64) error {
	err := s.DBStore.SaveRelease(ctx, appID, deployID, snap, source)
	if err == nil && source != 0 && s.afterSave != nil {
		s.afterSave()
	}
	return err
}

func TestRollbackDoesNotRepairLatePlacementDrift(t *testing.T) {
	for _, stage := range []string{"after-release-save", "before-service-apply"} {
		t.Run(stage, func(t *testing.T) {
			q, store, d, base, id := releaseFixture(t)
			if err := q.SetApplicationPlacement(t.Context(), db.SetApplicationPlacementParams{ID: id, PlacementMode: "pin", PlacementNodes: "A"}); err != nil {
				t.Fatal(err)
			}
			e := &placementReleaseEngine{releaseEngine: base, label: true}
			d.engine = e
			first := d.Enqueue(id, TriggerManual)
			runReleaseJob(t, d, first)
			requireDeployment(t, q, first, "done")
			applied, sets := len(e.deployed), e.sets
			wrapped := &placementReleaseStore{DBStore: store}
			d.store = wrapped
			rollback, err := d.EnqueueRollback(t.Context(), id, first)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "after-release-save" {
				wrapped.afterSave = func() { e.label = false }
			} else {
				e.driftBeforeApply = true
			}
			runReleaseJob(t, d, rollback)
			requireDeployment(t, q, rollback, "error")
			if e.label || e.sets != sets || len(e.deployed) != applied {
				t.Fatal("rollback repaired labels or mutated service after placement drift")
			}
		})
	}
}
