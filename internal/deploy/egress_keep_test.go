package deploy

import (
	"context"
	"testing"
)

// Every app deploy asks the engine to keep the running service's link egress
// attachments: a release that does not manage them (a rollback from the
// egress release) must not drop them, or the app's traffic leaves directly.
func TestDeployKeepsEgressAttachments(t *testing.T) {
	eng := &mockEngine{}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.deployed) != 1 || eng.deployed[0].KeepNetworkPrefix != "krill-egress-" {
		t.Fatalf("deployed specs %+v: want KeepNetworkPrefix krill-egress-", eng.deployed)
	}
}
