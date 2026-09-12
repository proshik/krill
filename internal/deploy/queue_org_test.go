package deploy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestEnqueueRefusesWhenOrgIsAtItsLimit mirrors TestEnqueueRefusesWhenAppHasOneInFlight
// but at the organization level: a single tenant must not be able to fill the
// shared build queue and stall every other org's deploys.
func TestEnqueueRefusesWhenOrgIsAtItsLimit(t *testing.T) {
	st := newFakeStore(imageApp()) // App ID 1
	st.runningByOrg = 2
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	d.SetMaxBuildsPerOrg(2)

	if id := d.Enqueue(1, "manual"); id != 0 {
		t.Fatalf("want refusal (0), got deployment id %d", id)
	}

	st.runningByOrg = 1
	if id := d.Enqueue(1, "manual"); id == 0 {
		t.Fatal("below the limit the deploy must be accepted")
	}
}

// TestEnqueueSkipsOrgCheckWhenCapNotSet verifies MaxBuildsPerOrg<=0 ("no cap")
// skips the per-org query entirely, rather than merely tolerating a large
// count: the underlying store call errors here, so a deploy only succeeds if
// the check truly never ran.
func TestEnqueueSkipsOrgCheckWhenCapNotSet(t *testing.T) {
	st := newFakeStore(imageApp())
	st.orgCountErr = errors.New("boom")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	// d.maxBuildsPerOrg defaults to 0 ("no cap") — SetMaxBuildsPerOrg is never called.

	if id := d.Enqueue(1, "manual"); id == 0 {
		t.Fatal("zero cap must skip the per-org check entirely, not fail on its error")
	}
}

// TestEnqueueRefusesWhenOrgCheckErrors verifies a failed per-org count query
// fails closed (refuses the deploy) rather than silently allowing it through,
// matching the existing per-app check's error handling.
func TestEnqueueRefusesWhenOrgCheckErrors(t *testing.T) {
	st := newFakeStore(imageApp())
	st.orgCountErr = errors.New("boom")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	d.SetMaxBuildsPerOrg(2)

	if id := d.Enqueue(1, "manual"); id != 0 {
		t.Fatalf("a per-org count error must refuse the deploy, got deployment id %d", id)
	}
}

// TestEnqueueSystemBypassesBothCaps pins the one caller that must not be
// throttled: the startup migration onto per-organization networks redeploys
// every service of an organization at once, and both caps exist to stop a
// tenant from monopolising the single build worker — neither is meant to apply
// to the control plane moving its own services. The per-org store call errors
// here, so the test also proves that check never ran rather than merely passed.
func TestEnqueueSystemBypassesBothCaps(t *testing.T) {
	st := newFakeStore(imageApp()) // App ID 1
	st.orgCountErr = errors.New("boom")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	d.SetMaxBuildsPerOrg(1)

	if id := d.EnqueueSystem(context.Background(), 1); id == 0 {
		t.Fatal("a system deploy must not be refused by the per-org cap")
	}
	// The first one is now in flight, which is exactly what the per-app guard
	// refuses a second deploy for.
	if id := d.Enqueue(1, "manual"); id != 0 {
		t.Fatalf("the per-app guard must still hold for ordinary callers, got %d", id)
	}
	if id := d.EnqueueSystem(context.Background(), 1); id == 0 {
		t.Fatal("a system deploy must not be refused by the per-app in-flight guard")
	}
}

// An organization with more applications than the queue is deep must not lose
// its tail: a dropped system deploy strands that service on the shared network
// with nothing to retry it. EnqueueSystem waits for room instead, bounded by
// its context — and while it waits it must not write failed deployment rows.
func TestEnqueueSystemWaitsForQueueRoom(t *testing.T) {
	st := newFakeStore(imageApp())
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	// No worker is started, so nothing drains: fill the queue to capacity with
	// one deploy per distinct app (the per-app guard allows one each).
	for i := 1; i <= cap(d.queue); i++ {
		if id := d.Enqueue(int64(i), "manual"); id == 0 {
			t.Fatalf("filling the queue: app %d was refused", i)
		}
	}
	rowsBefore := st.deploymentCount()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if id := d.EnqueueSystem(ctx, 9999); id != 0 {
		t.Fatalf("a full queue must not accept the deploy, got %d", id)
	}
	if n := st.deploymentCount(); n != rowsBefore {
		t.Fatalf("waiting for room must not create deployment rows: %d -> %d", rowsBefore, n)
	}

	// Room frees up: the same call succeeds.
	<-d.queue
	if id := d.EnqueueSystem(context.Background(), 9999); id == 0 {
		t.Fatal("with room in the queue the system deploy must be accepted")
	}
}
