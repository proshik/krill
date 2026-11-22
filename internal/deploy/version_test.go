package deploy

import (
	"context"
	"sync"
	"testing"
)

func TestImageVersion(t *testing.T) {
	const pinned = "ghcr.io/acme/demo@sha256:b1139b7c55e2f00dfeedbeef"
	for _, c := range []struct{ tag, ref, want string }{
		{"v1.8.3", pinned, "v1.8.3"},
		{"16.4.1-alpine", pinned, "16.4.1-alpine"},
		{"sha-1a09ec0", pinned, "sha-1a09ec0"},
		{"1.2", "ghcr.io/acme/demo:1.2", "1.2"},         // no digest resolved
		{"v1", pinned, "v1 (b1139b7c55e2)"},             // a major tag moves
		{"stable-2", pinned, "stable-2 (b1139b7c55e2)"}, // so does a channel
		{"16", pinned, "16 (b1139b7c55e2)"},
		{"latest", pinned, "latest (b1139b7c55e2)"},
		{"main", "docker.io/library/nginx@sha256:abc123", "main (abc123)"},
		{"latest", "nginx:latest", "latest"}, // digest lookup failed: the plain tag
		{"", pinned, ""},
	} {
		if got := imageVersion(c.tag, c.ref); got != c.want {
			t.Errorf("imageVersion(%q, %q) = %q, want %q", c.tag, c.ref, got, c.want)
		}
	}
}

func TestGitVersion(t *testing.T) {
	for _, c := range []struct{ branch, commit, want string }{
		{"master", "1a09ec0", "master@1a09ec0"},
		{"", "1a09ec0", "main@1a09ec0"},
		{"release/2", "", "release/2"},
	} {
		if got := gitVersion(c.branch, c.commit); got != c.want {
			t.Errorf("gitVersion(%q, %q) = %q, want %q", c.branch, c.commit, got, c.want)
		}
	}
}

// versionFakeStore is fakeStore with the optional version capability.
type versionFakeStore struct {
	*fakeStore
	vmu      sync.Mutex
	versions map[int64]string
}

func (s *versionFakeStore) SetDeploymentVersion(_ context.Context, id int64, v string) error {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	s.versions[id] = v
	return nil
}

func (s *versionFakeStore) DeploymentVersion(_ context.Context, id int64) (string, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	return s.versions[id], nil
}

func (s *versionFakeStore) version(id int64) string {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	return s.versions[id]
}

// A deploy records what it shipped: the mutable tag of an image app with the
// digest it resolved to, and branch@commit for a Dockerfile build.
func TestDeployRecordsVersion(t *testing.T) {
	eng := &digestMockEngine{digest: "docker.io/library/nginx@sha256:b1139b7c55e2aaaa"}
	app := imageApp()
	app.Tag = "latest"
	st := &versionFakeStore{fakeStore: newFakeStore(app), versions: map[int64]string{}}
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()
	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	if got := st.version(id); got != "latest (b1139b7c55e2)" {
		t.Fatalf("image deploy version = %q", got)
	}

	tagged := &versionFakeStore{fakeStore: newFakeStore(imageApp()), versions: map[int64]string{}}
	d2 := newDeployer(eng, &mockBuilder{}, tagged)
	d2.Start(context.Background())
	defer d2.Stop()
	id2, err := d2.EnqueueImageErr(1, "api", "ghcr.io/acme/demo", "v1.8.3")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return tagged.depStatus(id2) == "done" })
	if got := tagged.version(id2); got != "v1.8.3" {
		t.Fatalf("the job's tag is the version: %q", got)
	}

	df := &versionFakeStore{fakeStore: newFakeStore(dockerfileApp()), versions: map[int64]string{}}
	d3 := newDeployer(&mockEngine{}, &mockBuilder{commit: "1a09ec0"}, df)
	d3.Start(context.Background())
	defer d3.Stop()
	id3 := d3.Enqueue(2, "manual")
	waitFor(t, func() bool { return df.depStatus(id3) != "" })
	if got := df.version(id3); got != "main@1a09ec0" {
		t.Fatalf("dockerfile deploy version = %q (status %s, %s)", got, df.depStatus(id3), df.depError(id3))
	}
}

// A build that fails records no version.
func TestFailedBuildRecordsNoVersion(t *testing.T) {
	st := &versionFakeStore{fakeStore: newFakeStore(dockerfileApp()), versions: map[int64]string{}}
	d := newDeployer(&mockEngine{}, &mockBuilder{fail: true, commit: "1a09ec0"}, st)
	d.Start(context.Background())
	defer d.Stop()
	id := d.Enqueue(2, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	if got := st.version(id); got != "" {
		t.Fatalf("failed build recorded %q", got)
	}
}
