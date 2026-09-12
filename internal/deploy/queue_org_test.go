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

	if id := d.EnqueueSystem(1); id == 0 {
		t.Fatal("a system deploy must not be refused by the per-org cap")
	}
	// The first one is now in flight, which is exactly what the per-app guard
	// refuses a second deploy for.
	if id := d.Enqueue(1, "manual"); id != 0 {
		t.Fatalf("the per-app guard must still hold for ordinary callers, got %d", id)
	}
	if id := d.EnqueueSystem(1); id == 0 {
		t.Fatal("a system deploy must not be refused by the per-app in-flight guard")
	}
}
