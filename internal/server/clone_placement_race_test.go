package server_test

import (
	"context"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

type pausedPlacementBuild struct{ started, resume chan struct{} }

func (b *pausedPlacementBuild) Build(ctx context.Context, _ builder.BuildRequest, _ io.Writer) error {
	close(b.started)
	select {
	case <-b.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type placementRaceEngine struct {
	noopEngine
	mu      sync.Mutex
	labels  map[string]map[string]string
	applied int
}

func (e *placementRaceEngine) Nodes(context.Context) ([]docker.SwarmNode, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []docker.SwarmNode{}
	for id := range e.labels {
		out = append(out, docker.SwarmNode{ID: id})
	}
	return out, nil
}
func (e *placementRaceEngine) NodeSetLabel(_ context.Context, node, key, value string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.labels[node][key] = value
	return nil
}
func (e *placementRaceEngine) NodeDeleteLabel(_ context.Context, node, key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.labels[node], key)
	return nil
}
func (e *placementRaceEngine) ServiceDeploy(context.Context, docker.ServiceSpec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applied++
	return nil
}

func TestPlacementEditDuringBuildCannotRestoreCapturedNode(t *testing.T) {
	ctx := t.Context()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	os := org.NewService(q)
	uid := mkUser(t, q, "placement-race@fixture.test")
	o, err := os.CreateOrg(ctx, uid, "placement")
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.CreateProject(ctx, o.ID, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.CreateEnvironment(ctx, p.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: env.ID, Name: "web", SourceType: "dockerfile", GitUrl: "https://git.test/app", GitBranch: "main", DockerfilePath: "Dockerfile", Port: 80, Domain: "placement.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationPlacement(ctx, db.SetApplicationPlacementParams{ID: app.ID, PlacementMode: "global", PlacementNodes: "A"}); err != nil {
		t.Fatal(err)
	}
	key := "krill.place." + i64(app.ID)
	engine := &placementRaceEngine{labels: map[string]map[string]string{"A": {key: "1"}, "B": {}}}
	build := &pausedPlacementBuild{started: make(chan struct{}), resume: make(chan struct{})}
	hub := deploy.NewLogHub()
	dep := deploy.New(engine, build, deploy.NewDBStore(q, pool), hub, "fixture")
	dep.Start(ctx)
	defer dep.Stop()
	srv := server.New(config.Config{}, auth.NewService(q), os, q, dep, engine, hub, nil)
	id := dep.Enqueue(app.ID, deploy.TriggerManual)
	if id == 0 {
		t.Fatal("enqueue failed")
	}
	select {
	case <-build.started:
	case <-time.After(3 * time.Second):
		t.Fatal("build did not pause")
	}
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(env.ID) + "/apps/" + i64(app.ID)
	rec := postForm(t, srv.Router(), base+"/placement", loginAs(t, q, "placement-race@fixture.test"), url.Values{"placement_mode": {"global"}, "placement_nodes": {"B"}})
	close(build.resume)
	if hasErrFlash(rec) {
		t.Fatal("placement edit during build refused")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		d, err := q.GetDeployment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != "running" {
			if d.Status != "error" {
				t.Fatalf("stale build was applied: %s", d.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deploy did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.labels["A"][key] != "" || engine.labels["B"][key] != "1" || engine.applied != 0 {
		t.Fatalf("stale eligible nodes or service mutation: labels=%v applied=%d", engine.labels, engine.applied)
	}
}
