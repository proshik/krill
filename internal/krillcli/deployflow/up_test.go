package deployflow_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
)

// fakeDocker records every argv it is asked to run. "Recorded nothing" is an
// assertion several tests depend on.
type fakeDocker struct {
	calls    [][]string
	runErr   map[string]error
	outErr   map[string]error
	outValue string
}

func (d *fakeDocker) record(args []string) { d.calls = append(d.calls, args) }

func (d *fakeDocker) Run(_ context.Context, args ...string) error {
	d.record(args)
	return d.runErr[args[0]]
}

func (d *fakeDocker) Output(_ context.Context, args ...string) (string, error) {
	d.record(args)
	if err := d.outErr[args[0]+" "+args[1]]; err != nil {
		return "", err
	}
	if err := d.outErr[args[0]]; err != nil {
		return "", err
	}
	return d.outValue, nil
}

func (d *fakeDocker) Stream(_ context.Context, args ...string) (io.ReadCloser, error) {
	d.record(args)
	return io.NopCloser(strings.NewReader("")), nil
}

func (d *fakeDocker) verbs() []string {
	var out []string
	for _, c := range d.calls {
		out = append(out, strings.Join(c[:min(2, len(c))], " "))
	}
	return out
}

type fakeAPI struct {
	who        client.Whoami
	whoErr     error
	app        client.AppStatus
	appErr     error
	appAfter   *client.AppStatus // returned by the post-deploy status check
	appCalls   int
	deployErr  []error // consumed in order, one per Deploy call
	deployCall int
	deps       []client.Deployment
	statuses   []client.DeploymentDetail // consumed in order
	statusCall int
}

func (a *fakeAPI) Whoami(context.Context) (client.Whoami, error) { return a.who, a.whoErr }

func (a *fakeAPI) AppStatus(context.Context, string) (client.AppStatus, error) {
	a.appCalls++
	if a.appCalls > 1 && a.appAfter != nil {
		return *a.appAfter, nil
	}
	return a.app, a.appErr
}

func (a *fakeAPI) Deployments(context.Context, string, int) ([]client.Deployment, error) {
	return a.deps, nil
}

func (a *fakeAPI) Deployment(_ context.Context, id int64) (client.DeploymentDetail, error) {
	if a.statusCall < len(a.statuses) {
		d := a.statuses[a.statusCall]
		a.statusCall++
		return d, nil
	}
	if len(a.statuses) == 0 {
		return client.DeploymentDetail{Deployment: client.Deployment{ID: id, Status: "done"}}, nil
	}
	return a.statuses[len(a.statuses)-1], nil
}

func (a *fakeAPI) Deploy(context.Context, string, string) (client.Accepted, error) {
	a.deployCall++
	if a.deployCall <= len(a.deployErr) {
		if err := a.deployErr[a.deployCall-1]; err != nil {
			return client.Accepted{}, err
		}
	}
	return client.Accepted{DeploymentID: int64(400 + a.deployCall), Status: "running"}, nil
}

func okAPI() *fakeAPI {
	return &fakeAPI{
		who: client.Whoami{OrgName: "acme", Level: "write", Role: "owner", CanWrite: true},
		app: client.AppStatus{
			App: client.App{
				ID: 17, Path: "acme/production/bot", Name: "bot",
				SourceType: "image", Image: "ghcr.io/acme/bot", Tag: "v1", Status: "running",
				Domains: []string{"bot.example.com"},
			},
			Replicas: "1/1",
		},
	}
}

func okOptions() deployflow.Options {
	return deployflow.Options{
		Ref: "acme/production/bot", Repository: "ghcr.io/acme/bot",
		Platform: "linux/amd64", Tag: "v2", Dockerfile: "Dockerfile", Context: ".",
		Delivery: "registry", CanWriteHint: true, Timeout: time.Minute,
	}
}

func newFlow(api deployflow.API, d deployflow.Docker) (*deployflow.Flow, *strings.Builder) {
	var out strings.Builder
	return &deployflow.Flow{
		API: api, Docker: d, Out: &out,
		// Tests must not actually wait out the poll schedule.
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, &out
}

func TestDeployHappyPath(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{outValue: "sha256:abc"}
	f, out := newFlow(api, d)

	if err := f.Run(context.Background(), okOptions()); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"build --platform", "image inspect", "push --"}
	if got := d.verbs(); !equal(got, want) {
		t.Fatalf("docker calls = %v, want %v", got, want)
	}
	for _, sub := range []string{"✓ build", "✓ push", "✓ deploy", "✓ rollout", "bot.example.com"} {
		if !strings.Contains(out.String(), sub) {
			t.Fatalf("output missing %q:\n%s", sub, out)
		}
	}
}

// TestReadTokenFailsBeforeAnyRequest is the point of caching the level: a
// read token must not cost a four-minute build to discover.
func TestReadTokenFailsBeforeAnyRequest(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.CanWriteHint = false
	err := f.Run(context.Background(), o)
	if deployflow.CodeOf(err) != deployflow.ExitAuth {
		t.Fatalf("want ExitAuth, got %v (%v)", deployflow.CodeOf(err), err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("docker was invoked despite a read-only token: %v", d.verbs())
	}
	if api.appCalls != 0 {
		t.Fatal("the server was contacted despite a locally known read-only token")
	}
}

// TestImageMismatchStopsBeforeBuilding is the assertion the whole check
// ordering exists for. Zero docker calls proves the check ran first.
func TestImageMismatchStopsBeforeBuilding(t *testing.T) {
	api := okAPI()
	api.app.Image = "ghcr.io/acme/SOMETHING-ELSE"
	d := &fakeDocker{}
	f, _ := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitUsage {
		t.Fatalf("want ExitUsage, got %v (%v)", deployflow.CodeOf(err), err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("build started despite a repository mismatch: %v", d.verbs())
	}
	if !strings.Contains(err.Error(), "krill.yaml") || !strings.Contains(err.Error(), "Krill UI") {
		t.Fatalf("the error must name both fixes, got: %v", err)
	}
}

func TestImageMismatchCanBeOverridden(t *testing.T) {
	api := okAPI()
	api.app.Image = "mirror.internal/acme/bot"
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.AllowImageMismatch = true
	if err := f.Run(context.Background(), o); err != nil {
		t.Fatalf("--allow-image-mismatch should proceed: %v", err)
	}
}

// TestEquivalentRepositorySpellingsAreNotAMismatch guards the normalization:
// a false alarm here blocks a correct deploy.
func TestEquivalentRepositorySpellingsAreNotAMismatch(t *testing.T) {
	api := okAPI()
	api.app.Image = "docker.io/library/nginx"
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.Repository = "nginx"
	if err := f.Run(context.Background(), o); err != nil {
		t.Fatalf("nginx and docker.io/library/nginx are one repository: %v", err)
	}
}

func TestDockerfileAppIsRefusedBeforeBuilding(t *testing.T) {
	api := okAPI()
	api.app.SourceType = "dockerfile"
	d := &fakeDocker{}
	f, _ := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitUsage {
		t.Fatalf("want ExitUsage, got %v", err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("build started for a dockerfile app: %v", d.verbs())
	}
	if !strings.Contains(err.Error(), "rebuild") {
		t.Fatalf("the error should point at rebuild, got: %v", err)
	}
}

func TestBuildFailureStopsBeforePush(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{runErr: map[string]error{"build": errors.New("denied: requested access to the resource is denied")}}
	f, _ := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, c := range d.calls {
		if c[0] == "push" {
			t.Fatal("push ran after a failed build")
		}
	}
	if api.deployCall != 0 {
		t.Fatal("deploy was called after a failed build")
	}
	// A known docker failure must arrive with its fix attached.
	if !strings.Contains(err.Error(), "docker login") {
		t.Fatalf("expected an actionable hint, got: %v", err)
	}
}

// TestBuildxImageNotLoadedIsCaught covers the case a successful build still
// leaves nothing to push: a buildx container driver keeps the image.
func TestBuildxImageNotLoadedIsCaught(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{outErr: map[string]error{"image inspect": errors.New("Error: No such image")}}
	f, _ := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if err == nil {
		t.Fatal("want an error when the built image is not in the local store")
	}
	if !strings.Contains(err.Error(), "buildx") {
		t.Fatalf("the hint should name the buildx driver, got: %v", err)
	}
	if api.deployCall != 0 {
		t.Fatal("deploy ran despite there being no image to push")
	}
}

func TestPushFailureStopsBeforeDeploy(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{
		outValue: "sha256:abc",
		runErr:   map[string]error{"push": errors.New("name unknown: repository name not known to registry")},
	}
	f, _ := newFlow(api, d)

	if err := f.Run(context.Background(), okOptions()); err == nil {
		t.Fatal("want an error")
	}
	if api.deployCall != 0 {
		t.Fatal("deploy was called after a failed push")
	}
}

// TestConflictWaitsAndDeploysExactlyOnce is the load-bearing one. Every
// accepted deploy rewrites the application's tag and the API cannot undo
// that, so a conflict must never turn into a retry loop.
func TestConflictWaitsAndDeploysExactlyOnce(t *testing.T) {
	api := okAPI()
	api.deployErr = []error{&client.APIError{HTTPStatus: 409, Code: "conflict", Message: "already in progress"}}
	api.deps = []client.Deployment{{ID: 99, Status: "running"}}
	api.statuses = []client.DeploymentDetail{
		{Deployment: client.Deployment{ID: 99, Status: "running"}},
		{Deployment: client.Deployment{ID: 99, Status: "done"}},
		{Deployment: client.Deployment{ID: 402, Status: "done"}},
	}
	d := &fakeDocker{outValue: "sha256:abc"}
	f, out := newFlow(api, d)

	if err := f.Run(context.Background(), okOptions()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if api.deployCall != 2 {
		t.Fatalf("Deploy was called %d times, want exactly 2 (the refused one and one retry after waiting)", api.deployCall)
	}
	if !strings.Contains(out.String(), "waiting") {
		t.Fatalf("the wait should be visible to the user:\n%s", out)
	}
}

func TestConflictWithNoWaitExitsBusy(t *testing.T) {
	api := okAPI()
	api.deployErr = []error{&client.APIError{HTTPStatus: 409, Code: "conflict"}}
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.NoWaitForLock = true
	err := f.Run(context.Background(), o)
	if deployflow.CodeOf(err) != deployflow.ExitBusy {
		t.Fatalf("want ExitBusy, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if api.deployCall != 1 {
		t.Fatalf("Deploy was retried despite --no-wait-for-lock: %d calls", api.deployCall)
	}
}

func TestFailedDeploymentExitsFailed(t *testing.T) {
	api := okAPI()
	api.statuses = []client.DeploymentDetail{
		{Deployment: client.Deployment{ID: 401, Status: "error"}, LogTail: "pull access denied\n"},
	}
	d := &fakeDocker{outValue: "sha256:abc"}
	f, out := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitFailed {
		t.Fatalf("want ExitFailed, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if !strings.Contains(out.String(), "pull access denied") {
		t.Fatalf("the failure log should be shown:\n%s", out)
	}
}

// TestGreenDeployWithDeadContainerIsNotSuccess covers the case the runbook
// calls out: the job finished, the container started and exited.
func TestGreenDeployWithDeadContainerIsNotSuccess(t *testing.T) {
	api := okAPI()
	after := api.app
	after.Replicas = "0/1"
	api.appAfter = &after
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitNotRunning {
		t.Fatalf("want ExitNotRunning, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "krill-cli logs") {
		t.Fatalf("the error should say how to look: %v", err)
	}
}

// TestTimeoutIsNotSuccess — a watch that gives up must not report 0, and must
// say how to rejoin.
func TestTimeoutIsNotSuccess(t *testing.T) {
	api := okAPI()
	api.statuses = []client.DeploymentDetail{{Deployment: client.Deployment{ID: 401, Status: "running"}}}
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.Timeout = time.Nanosecond
	err := f.Run(context.Background(), o)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "--watch") {
		t.Fatalf("the error should say how to rejoin: %v", err)
	}
}

// TestRateLimitedPollIsSkippedNotFailed — the budget is shared, so a 429 mid
// watch means wait, not fail.
func TestRateLimitedPollIsSkippedNotFailed(t *testing.T) {
	api := &rateLimitedAPI{fakeAPI: okAPI(), limitFirst: 2}
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	if err := f.Run(context.Background(), okOptions()); err != nil {
		t.Fatalf("a 429 during the watch must not fail the deploy: %v", err)
	}
}

type rateLimitedAPI struct {
	*fakeAPI
	limitFirst int
	seen       int
}

func (a *rateLimitedAPI) Deployment(ctx context.Context, id int64) (client.DeploymentDetail, error) {
	a.seen++
	if a.seen <= a.limitFirst {
		return client.DeploymentDetail{}, &client.APIError{HTTPStatus: 429, Code: "rate_limited", RetryAfter: time.Minute}
	}
	return a.fakeAPI.Deployment(ctx, id)
}

func TestDryRunTouchesNothing(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{}
	f, out := newFlow(api, d)

	o := okOptions()
	o.DryRun = true
	if err := f.Run(context.Background(), o); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("dry run invoked docker: %v", d.verbs())
	}
	if api.deployCall != 0 {
		t.Fatal("dry run deployed")
	}
	// The printed plan must be the real argv, since debugging is what it is for.
	for _, sub := range []string{"docker build", "--platform linux/amd64", "ghcr.io/acme/bot:v2", "docker push"} {
		if !strings.Contains(out.String(), sub) {
			t.Fatalf("dry run should print %q:\n%s", sub, out)
		}
	}
}

func TestUnauthorizedExitsAuth(t *testing.T) {
	api := okAPI()
	api.whoErr = &client.APIError{HTTPStatus: 401, Code: "unauthorized", Message: "valid bearer token required"}
	d := &fakeDocker{}
	f, _ := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitAuth {
		t.Fatalf("want ExitAuth, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if len(d.calls) != 0 {
		t.Fatal("docker ran despite a rejected token")
	}
}

func TestMissingAppNamesTheReservedVerbTrap(t *testing.T) {
	api := okAPI()
	api.appErr = &client.APIError{HTTPStatus: 404, Code: "not_found", Message: "no such application"}
	d := &fakeDocker{}
	f, _ := newFlow(api, d)

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitUsage {
		t.Fatalf("want ExitUsage, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "numeric id") {
		t.Fatalf("the error should mention the reserved-verb case: %v", err)
	}
}

func TestUploadDeliveryIsReportedAsUnavailable(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.Delivery = "upload"
	err := f.Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("upload delivery should fail with a pointer to registry, got %v", err)
	}
	if api.deployCall != 0 {
		t.Fatal("deploy ran despite delivery being unavailable")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
