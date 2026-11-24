package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// resolvingEngine holds an image deploy while it resolves the image's digest,
// the way a slow registry does, and counts service writes.
type resolvingEngine struct {
	noopEngine
	mu       sync.Mutex
	entered  chan struct{}
	deploys  int
	scales   int
	scaledTo []uint64
}

func (e *resolvingEngine) ResolveDigest(ctx context.Context, _, _ string) (string, error) {
	e.mu.Lock()
	if e.entered != nil {
		close(e.entered)
		e.entered = nil
	}
	e.mu.Unlock()
	<-ctx.Done()
	return "", ctx.Err()
}

func (e *resolvingEngine) ServiceDeploy(context.Context, docker.ServiceSpec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deploys++
	return nil
}

func (e *resolvingEngine) ServiceScale(_ context.Context, _ string, n uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scales++
	e.scaledTo = append(e.scaledTo, n)
	return nil
}

func (e *resolvingEngine) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deploys, e.scales
}

func latestDeployment(t *testing.T, q *db.Queries, appID int64) db.Deployment {
	t.Helper()
	ds, err := q.ListDeploymentsByApplication(context.Background(), appID)
	if err != nil || len(ds) == 0 {
		t.Fatalf("list deployments: %v (%d)", err, len(ds))
	}
	return ds[0]
}

// Stop pressed while a deploy still resolves its image cancels the deploy: the
// deployment says who stopped it, the app is idle, a new deploy is accepted at
// once, and the cancelled deploy never writes the service. Before the fix the
// Stop scaled a service the deploy had not created yet and the deploy then
// brought the app up.
func TestStopCancelsDeployNotYetApplied(t *testing.T) {
	for _, via := range []string{"ui", "api"} {
		t.Run(via, func(t *testing.T) {
			eng := &resolvingEngine{entered: make(chan struct{})}
			entered := eng.entered
			h, q, orgSvc, _, _ := buildDeployServer(t, nil, eng)
			fx := seedAPISuccessFixture(t, q, orgSvc)
			cookie := loginAs(t, q, "api-success@k.local")
			app, err := q.GetApplication(context.Background(), fx.appID)
			if err != nil {
				t.Fatal(err)
			}
			env, _ := q.GetEnvironment(context.Background(), app.EnvironmentID)
			base := "/orgs/" + i64(fx.orgID) + "/projects/" + i64(env.ProjectID) + "/environments/" + i64(env.ID) + "/apps/" + i64(fx.appID)

			if rec := postForm(t, h, base+"/deploy", cookie, url.Values{}); rec.Code != http.StatusSeeOther {
				t.Fatalf("deploy: %d", rec.Code)
			}
			<-entered

			switch via {
			case "ui":
				if rec := postForm(t, h, base+"/stop", cookie, url.Values{}); rec.Code != http.StatusSeeOther {
					t.Fatalf("stop: %d", rec.Code)
				}
			case "api":
				req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+i64(fx.appID)+"/stop", strings.NewReader("{}"))
				req.Header.Set("Authorization", "Bearer "+fx.token)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("api stop: %d %s", rec.Code, rec.Body.String())
				}
			}

			// Recorded by the stop itself, before it returned.
			d := latestDeployment(t, q, fx.appID)
			if d.Status != "error" || !strings.Contains(d.ErrorMessage, "stopped by api-success@k.local") ||
				!strings.HasSuffix(d.ErrorMessage, "before it was applied") {
				t.Fatalf("deployment = %s %v", d.Status, d.ErrorMessage)
			}
			if !strings.Contains(d.Log, "before it was applied") {
				t.Fatalf("deploy log does not say it was stopped: %q", d.Log)
			}
			got, _ := q.GetApplication(context.Background(), fx.appID)
			if got.Status != "idle" {
				t.Fatalf("app status = %q, want idle", got.Status)
			}
			if n, _ := q.CountRunningDeploymentsByApplication(context.Background(), fx.appID); n != 0 {
				t.Fatalf("%d deployments still in flight after the stop", n)
			}
			// The worker ran on to the end of the cancelled deploy by now if it
			// was going to apply it: give it the chance, then look.
			time.Sleep(200 * time.Millisecond)
			deploys, scales := eng.counts()
			if deploys != 0 || scales != 1 {
				t.Fatalf("service writes: %d deploys, %d scales; want 0 and 1", deploys, scales)
			}
			if d2 := latestDeployment(t, q, fx.appID); d2.Status != "error" || d2.ErrorMessage != d.ErrorMessage {
				t.Fatalf("stopped deployment rewritten: %s %v", d2.Status, d2.ErrorMessage)
			}
		})
	}
}
