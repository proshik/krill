package deploy

import (
	"errors"
	"testing"
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
