package deployflow_test

import (
	"context"
	"errors"
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
	// statusErrs are returned by the first len(statusErrs) Deployment calls;
	// statusErrAlways is returned by every one of them.
	statusErrs      []error
	statusErrAlways error
	// appAfterErr fails the post-rollout status check specifically.
	appAfterErr error
}

func (a *fakeAPI) Whoami(context.Context) (client.Whoami, error) { return a.who, a.whoErr }

func (a *fakeAPI) AppStatus(context.Context, string) (client.AppStatus, error) {
	a.appCalls++
	if a.appCalls > 1 {
		if a.appAfterErr != nil {
			return client.AppStatus{}, a.appAfterErr
		}
		if a.appAfter != nil {
			return *a.appAfter, nil
		}
	}
	return a.app, a.appErr
}

func (a *fakeAPI) Deployments(context.Context, string, int) ([]client.Deployment, error) {
	return a.deps, nil
}

func (a *fakeAPI) Deployment(_ context.Context, id int64) (client.DeploymentDetail, error) {
	call := a.statusCall
	a.statusCall++
	if a.statusErrAlways != nil {
		return client.DeploymentDetail{}, a.statusErrAlways
	}
	if call < len(a.statusErrs) && a.statusErrs[call] != nil {
		return client.DeploymentDetail{}, a.statusErrs[call]
	}
	if call < len(a.statuses) {
		return a.statuses[call], nil
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

// TestUploadDeliveryIsRefusedBeforeBuilding: an unimplemented delivery mode is
// the most certain failure there is, so it must be reported before the build
// rather than after it. The zero docker calls are the whole assertion — the
// refusal used to live past the build, which is exactly the four-minute wait
// followed by a rejection that this command's ordering promises to prevent.
func TestUploadDeliveryIsRefusedBeforeBuilding(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{outValue: "sha256:abc"}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.Delivery = "upload"
	err := f.Run(context.Background(), o)
	if deployflow.CodeOf(err) != deployflow.ExitUsage {
		t.Fatalf("want ExitUsage, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "registry") {
		t.Fatalf("the refusal should point at the registry mode, got %v", err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("docker ran before the delivery mode was refused: %v", d.verbs())
	}
	if api.deployCall != 0 {
		t.Fatal("deploy ran despite delivery being unavailable")
	}
}

// TestUploadDeliveryIsRefusedEvenOnDryRun: a dry run that prints a plan and
// exits 0 tells the user the configuration is fine, which is the opposite of
// true.
func TestUploadDeliveryIsRefusedEvenOnDryRun(t *testing.T) {
	f, _ := newFlow(okAPI(), &fakeDocker{})
	o := okOptions()
	o.Delivery, o.DryRun = "upload", true
	if err := f.Run(context.Background(), o); err == nil {
		t.Fatal("a dry run with an unavailable delivery mode reported success")
	}
}

// TestSkipPushRefusesAnImageMissingFromTheRegistry pins the irreversibility:
// deploying a tag the registry does not have moves the application row to it,
// and no API operation moves it back — so the web UI's Deploy button fails on
// it too, forever. A warning was not enough.
func TestSkipPushRefusesAnImageMissingFromTheRegistry(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{outValue: "sha256:abc", outErr: map[string]error{
		"manifest inspect": errors.New("manifest unknown"),
	}}
	f, _ := newFlow(api, d)

	o := okOptions()
	o.SkipBuild, o.SkipPush = true, true
	err := f.Run(context.Background(), o)
	if deployflow.CodeOf(err) != deployflow.ExitUsage {
		t.Fatalf("want ExitUsage, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if api.deployCall != 0 {
		t.Fatal("the app was retagged to an image the registry does not have")
	}
	if !strings.Contains(err.Error(), "--allow-missing-image") {
		t.Fatalf("the refusal should name its escape hatch, got %v", err)
	}
}

func TestSkipPushProceedsWithAllowMissingImage(t *testing.T) {
	api := okAPI()
	d := &fakeDocker{outValue: "sha256:abc", outErr: map[string]error{
		"manifest inspect": errors.New("manifest unknown"),
	}}
	f, out := newFlow(api, d)

	o := okOptions()
	o.SkipBuild, o.SkipPush, o.AllowMissingImage = true, true, true
	if err := f.Run(context.Background(), o); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "continuing anyway") {
		t.Fatalf("the override should still say what it overrode:\n%s", out)
	}
}

// TestWatchStopsWhenTheServerKeepsThrottling is the bug this ordering exists
// for: the retryable branch used to `continue` PAST the only deadline check,
// so a 429 or 503 on every poll looped forever and --timeout never fired.
func TestWatchStopsWhenTheServerKeepsThrottling(t *testing.T) {
	api := okAPI()
	api.statusErrAlways = &client.APIError{HTTPStatus: 503, Code: "unavailable", Message: "try later"}
	d := &fakeDocker{outValue: "sha256:abc"}

	var out strings.Builder
	f := &deployflow.Flow{
		API: api, Docker: d, Out: &out,
		// A real, tiny sleep: the deadline is wall-clock, so a no-op sleep
		// would make the loop unbounded no matter what the code does.
		Sleep: func(context.Context, time.Duration) error {
			time.Sleep(2 * time.Millisecond)
			return nil
		},
	}
	o := okOptions()
	o.Timeout = 40 * time.Millisecond

	err := f.Run(context.Background(), o)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
}

// TestWatchSurvivesATransportBlip: the deployment runs on the SERVER, so this
// machine losing its connection for a poll or two says nothing about it.
func TestWatchSurvivesATransportBlip(t *testing.T) {
	api := okAPI()
	api.statusErrs = []error{errors.New("cannot reach https://k: connection reset"), errors.New("cannot reach https://k: i/o timeout")}
	f, _ := newFlow(api, &fakeDocker{outValue: "sha256:abc"})

	if err := f.Run(context.Background(), okOptions()); err != nil {
		t.Fatalf("a dropped connection was reported as a failure: %v", err)
	}
}

// TestLostContactIsUnknownNotFailed: when it never comes back, the answer is
// "unknown" (exit 4), never "the deployment failed" (exit 1) — CI acting on
// the latter reports broken code for a deploy that succeeded.
func TestLostContactIsUnknownNotFailed(t *testing.T) {
	api := okAPI()
	api.statusErrAlways = errors.New("cannot reach https://k: no route to host")
	f, _ := newFlow(api, &fakeDocker{outValue: "sha256:abc"})

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "--watch") {
		t.Fatalf("the message should say how to rejoin, got %v", err)
	}
}

// TestUnreadableRolloutIsNotSuccess: the post-deploy status check exists to
// catch a container that started and died. When the check itself fails, the
// answer is unknown — reporting exit 0 marks a dead service green.
func TestUnreadableRolloutIsNotSuccess(t *testing.T) {
	api := okAPI()
	api.appAfterErr = errors.New("cannot reach https://k: connection refused")
	f, out := newFlow(api, &fakeDocker{outValue: "sha256:abc"})

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if strings.Contains(out.String(), "✓ rollout") {
		t.Fatalf("an unread rollout was printed as confirmed:\n%s", out)
	}
}

// TestUnreadableReplicasAreNotReportedAsCrashed: "?/1" is what the API sends
// when docker has no service to ask, which is not the same as a container that
// exited — and the difference matters, because one of them is a diagnosis.
func TestUnreadableReplicasAreNotReportedAsCrashed(t *testing.T) {
	api := okAPI()
	after := api.app
	after.Replicas = "?/1"
	api.appAfter = &after
	f, _ := newFlow(api, &fakeDocker{outValue: "sha256:abc"})

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
}

// TestNotRunningNamesBothCauses: 0/1 after a finished deployment has two
// causes and only the log tells them apart. Asserting the more dramatic one
// sends people to debug a crash that never happened.
func TestNotRunningNamesBothCauses(t *testing.T) {
	api := okAPI()
	after := api.app
	after.Replicas = "0/1"
	api.appAfter = &after
	f, _ := newFlow(api, &fakeDocker{outValue: "sha256:abc"})

	err := f.Run(context.Background(), okOptions())
	if deployflow.CodeOf(err) != deployflow.ExitNotRunning {
		t.Fatalf("want ExitNotRunning, got %d (%v)", deployflow.CodeOf(err), err)
	}
	for _, sub := range []string{"started and exited", "still starting"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("the message should mention %q, got %v", sub, err)
		}
	}
}

// TestDomainsArePrintedWithoutAScheme: the wire carries host names only — no
// exposed flag, no TLS flag — and a new app's automatic domain is created
// unexposed, with Traefik told not to route it. An https:// link there cannot
// work, and a link that cannot work reads as a broken deploy.
func TestDomainsArePrintedWithoutAScheme(t *testing.T) {
	f, out := newFlow(okAPI(), &fakeDocker{outValue: "sha256:abc"})
	if err := f.Run(context.Background(), okOptions()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out.String(), "https://") {
		t.Fatalf("a scheme was invented for a domain whose exposure is unknown:\n%s", out)
	}
	if !strings.Contains(out.String(), "bot.example.com") {
		t.Fatalf("the domain should still be listed:\n%s", out)
	}
}

// TestThrottledDeployIsResent: 429 and 503 mean the request was never looked
// at, so resending cannot produce a second deployment — and not resending
// throws away a completed build.
func TestThrottledDeployIsResent(t *testing.T) {
	api := okAPI()
	api.deployErr = []error{&client.APIError{HTTPStatus: 429, Code: "rate_limited", Message: "slow down"}}
	f, _ := newFlow(api, &fakeDocker{outValue: "sha256:abc"})

	if err := f.Run(context.Background(), okOptions()); err != nil {
		t.Fatalf("a throttled deploy was not retried: %v", err)
	}
	if api.deployCall != 2 {
		t.Fatalf("Deploy called %d times, want 2", api.deployCall)
	}
}

// TestPersistentThrottlingGivesUp bounds the retry: a server that answers 429
// forever must end the command, not the user's patience.
func TestPersistentThrottlingGivesUp(t *testing.T) {
	api := okAPI()
	for i := 0; i < 20; i++ {
		api.deployErr = append(api.deployErr, &client.APIError{HTTPStatus: 429, Code: "rate_limited", Message: "slow down"})
	}
	f, _ := newFlow(api, &fakeDocker{outValue: "sha256:abc"})

	if err := f.Run(context.Background(), okOptions()); err == nil {
		t.Fatal("persistent throttling reported success")
	}
	if api.deployCall > 6 {
		t.Fatalf("Deploy was resent %d times, want a bounded number", api.deployCall)
	}
}

// TestDockerfileAppPointsAtACommandThatExists guards the advice, not the
// refusal: the message used to name `krill-cli rebuild`, which was not a
// registered command, so following it printed "unknown command".
func TestDockerfileAppPointsAtACommandThatExists(t *testing.T) {
	api := okAPI()
	api.app.SourceType = "dockerfile"
	f, _ := newFlow(api, &fakeDocker{})

	err := f.Run(context.Background(), okOptions())
	if err == nil || !strings.Contains(err.Error(), "rebuild") {
		t.Fatalf("want a pointer to rebuild, got %v", err)
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
