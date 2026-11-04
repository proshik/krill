package server_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

type releaseFixtureBuilder struct {
	cli    *client.Client
	images map[string]string
	mu     sync.Mutex
	tags   []string
}

func (b *releaseFixtureBuilder) Build(ctx context.Context, r builder.BuildRequest, w io.Writer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	source, ok := b.images[r.GitBranch]
	if !ok {
		return fmt.Errorf("unknown fixture branch")
	}
	if err := b.cli.ImageTag(ctx, source, r.ImageTag); err != nil {
		return err
	}
	b.tags = append(b.tags, r.ImageTag)
	fmt.Fprintln(w, "fixture build completed")
	return nil
}

// TestReleaseBrowser exposes the real router, deployment worker, PostgreSQL and
// Docker engine on a disposable fixture. It never starts Krill's global reconcilers.
// KRILL_RELEASE_BROWSER=1 enables it; POST /_test/stop tears it down.
func TestReleaseBrowser(t *testing.T) {
	if os.Getenv("KRILL_RELEASE_BROWSER") != "1" {
		t.Skip("opt-in real Docker/browser fixture")
	}
	secret.Init("disposable-release-browser-key")
	defer secret.Init("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("krill-release-browser-%d", time.Now().UnixMilli())
	if _, err := eng.NetworkEnsure(ctx, prefix); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for i := 0; i < 20; i++ {
			if err := eng.NetworkRemove(context.Background(), prefix); err == nil {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()
	b := &releaseFixtureBuilder{cli: cli, images: map[string]string{}}
	defer func() {
		for _, tag := range append(b.tags, b.images["v1"], b.images["v2"]) {
			if tag != "" {
				cli.ImageRemove(context.Background(), tag, image.RemoveOptions{Force: true})
			}
		}
	}()
	for _, version := range []string{"v1", "v2"} {
		var archive bytes.Buffer
		tw := tar.NewWriter(&archive)
		body := "FROM traefik:v3.6.1\nRUN echo " + version + " > /release-version\nENTRYPOINT [\"/bin/sh\",\"-c\"]\nCMD [\"exec sleep 3600\"]\n"
		if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
		tw.Close()
		tag := prefix + ":" + version
		response, err := cli.ImageBuild(ctx, &archive, build.ImageBuildOptions{Tags: []string{tag}, Remove: true})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if _, _, err := cli.ImageInspectWithRaw(ctx, tag); err != nil {
			t.Fatal(err)
		}
		b.images[version] = tag
	}
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	owner := mkUser(t, q, "release-owner@k.local")
	orgSvc := org.NewService(q)
	o, err := orgSvc.CreateOrg(ctx, owner, "Release fixture")
	if err != nil {
		t.Fatal(err)
	}
	p, err := orgSvc.CreateProject(ctx, o.ID, "Project", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "SELECT setval('applications_id_seq', $1)", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE organizations SET network_name=$1 WHERE id=$2", prefix, o.ID); err != nil {
		t.Fatal(err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: env.ID, Name: "web", SourceType: "dockerfile", GitUrl: "https://example.invalid/fixture.git", GitBranch: "v1", DockerfilePath: "Dockerfile", Port: 8080, Domain: prefix + ".invalid", EnvText: "# release v1\nPUBLIC=one\nTOKEN=old-secret"})
	if err != nil {
		t.Fatal(err)
	}
	q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: a.ID, EnvText: a.EnvText, EnvPublicKeys: []string{"PUBLIC"}})
	q.UpdateApplicationAdvanced(ctx, db.UpdateApplicationAdvancedParams{ID: a.ID, Replicas: 1, RestartCondition: "any", PredeployCommand: "test -f /release-version; printf '%s' \"$TOKEN\""})
	defer func() { eng.ServiceRemove(context.Background(), docker.ServiceName(a.ID)); time.Sleep(2 * time.Second) }()
	hub := deploy.NewLogHub()
	d := deploy.New(eng, b, deploy.NewDBStore(q, pool), hub, prefix)
	d.SetResourceDefaults(64<<20, 100000000)
	d.Start(ctx)
	defer d.Stop()
	cfg := config.Config{Network: prefix, BaseDomain: "example.invalid", AllowPrivateEgress: true}
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, d, eng, hub, dbservice.New(eng, dbservice.NewDBStore(q), hub, prefix))
	stop := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_test/stop", func(w http.ResponseWriter, r *http.Request) {
		select {
		case stop <- struct{}{}:
		default:
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /_test/state", func(w http.ResponseWriter, r *http.Request) {
		app, err := q.GetApplication(r.Context(), a.ID)
		if err != nil {
			http.Error(w, "fixture database error", 500)
			return
		}
		deps, _ := q.ListDeploymentsByApplication(r.Context(), a.ID)
		services, _ := cli.ServiceList(r.Context(), swarm.ServiceListOptions{})
		var spec any
		for _, service := range services {
			if service.Spec.Name == docker.ServiceName(a.ID) {
				spec = service
			}
		}
		progress, _ := eng.ServiceProgress(r.Context(), docker.ServiceName(a.ID), nil)
		var runtime bytes.Buffer
		eng.Exec(r.Context(), docker.ServiceName(a.ID), []string{"/bin/sh", "-c", "cat /release-version; printf '%s' \"$TOKEN\""}, nil, nil, &runtime)
		b.mu.Lock()
		count := len(b.tags)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"app": app, "deployments": deps, "service": spec, "progress": progress, "runtime": runtime.String(), "builds": count})
	})
	mux.Handle("/", srv.Router())
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	base := fmt.Sprintf("/orgs/%d/projects/%d/environments/%d/apps/%d", o.ID, p.ID, env.ID, a.ID)
	t.Logf("RELEASE_BROWSER_URL %s%s login release-owner@k.local / pw", httpServer.URL, base)
	select {
	case <-stop:
	case <-time.After(25 * time.Minute):
		t.Fatal("browser fixture timeout")
	}
}
