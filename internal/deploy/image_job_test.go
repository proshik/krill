package deploy

import (
	"context"
	"testing"
)

// The worker picks a job up later than it was queued. A deploy that asked for
// a tag must ship that tag even when the application row has been rewritten in
// the meantime — before the fix it read the row and shipped whichever tag was
// written last.
func TestEnqueueImageShipsTheTagItCarries(t *testing.T) {
	eng := &mockEngine{}
	st := newFakeStore(imageApp()) // nginx:alpine
	d := newDeployer(eng, &mockBuilder{}, st)

	id := d.EnqueueImage(1, TriggerManual, "nginx", "1.27")
	if id == 0 {
		t.Fatal("deploy refused")
	}
	if got := st.setImages; len(got) != 1 || got[0] != (ImageRef{Image: "nginx", Tag: "1.27"}) {
		t.Fatalf("accepted deploy must record its image on the app, got %+v", got)
	}
	// Another writer moves the row before the worker gets to the job.
	st.mu.Lock()
	st.app.Tag = "1.28"
	st.mu.Unlock()

	d.Start(context.Background())
	defer d.Stop()
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.deployed) != 1 || eng.deployed[0].Image != "nginx:1.27" {
		t.Fatalf("deployed = %+v, want the job's nginx:1.27", eng.deployed)
	}
}

// A refused deploy must leave the application's image alone: the next deploy
// from anywhere, the web UI included, would otherwise ship a tag nobody
// deployed.
func TestEnqueueImageRefusedLeavesTheAppImage(t *testing.T) {
	st := newFakeStore(imageApp())
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)

	if id := d.Enqueue(1, TriggerManual); id == 0 {
		t.Fatal("first deploy refused")
	}
	if id := d.EnqueueImage(1, TriggerAPI, "nginx", "1.27"); id != 0 {
		t.Fatalf("second deploy of the same app must be refused, got %d", id)
	}
	if len(st.setImages) != 0 {
		t.Fatalf("a refused deploy wrote the app image: %+v", st.setImages)
	}
}

// A dockerfile app has no image of its own; a stray reference in the job must
// not replace the tag the build produces.
func TestEnqueueImageIgnoredForDockerfileApps(t *testing.T) {
	eng := &mockEngine{}
	st := newFakeStore(dockerfileApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.EnqueueImage(2, TriggerManual, "nginx", "1.27")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.deployed) != 1 || eng.deployed[0].Image == "nginx:1.27" {
		t.Fatalf("deployed = %+v, want the built image", eng.deployed)
	}
}

func TestEnqueueSystemRecordsTheSystemTrigger(t *testing.T) {
	st := newFakeStore(imageApp())
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	id := d.EnqueueSystem(context.Background(), 1)
	if id == 0 {
		t.Fatal("system deploy refused")
	}
	if got := st.triggers[id]; got != TriggerSystem {
		t.Fatalf("trigger = %q, want %q", got, TriggerSystem)
	}
}
