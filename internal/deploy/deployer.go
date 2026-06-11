package deploy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// App — application representation for deployment.
type App struct {
	ID             int64
	Name           string
	Image          string
	Tag            string
	Domain         string
	Port           int32
	Env            map[string]string
	SourceType     string // "image" | "dockerfile"
	GitURL         string
	GitBranch      string
	DockerfilePath string
	Domains        []traefik.Domain
	RegistryAuth   string
	Args           []string // container command override (CMD), e.g. ["start-dev"]

	Replicas           uint64
	MemoryLimitBytes   int64
	NanoCPUs           int64
	RestartCondition   string
	RestartMaxAttempts uint64
	Healthcheck        *docker.HealthcheckSpec
}

// Store — what the deployer needs from the store.
type Store interface {
	GetApplication(ctx context.Context, id int64) (App, error)
	GetDeploymentApp(ctx context.Context, deployID int64) (App, error)
	SetStatus(ctx context.Context, id int64, status string) error
	CreateDeployment(ctx context.Context, appID int64, trigger string) (int64, error)
	FinishDeployment(ctx context.Context, deployID int64, status, imageTag, errMsg, log string) error
}

const jobTimeout = 10 * time.Minute // a build can take longer than a pull

var (
	convergeTimeout      = 180 * time.Second
	convergePollInterval = 1 * time.Second
)

// SetConvergeTimeout overrides the deploy convergence deadline (wired from
// config at startup). A healthcheck's start_period extends it per-deploy.
func SetConvergeTimeout(d time.Duration) {
	if d > 0 {
		convergeTimeout = d
	}
}

// job is one queued unit of work: a deployment plus build options.
type job struct {
	deployID int64
	noCache  bool // force docker build --no-cache (Rebuild)
}

// Notifier is the optional sink for deploy-failure alerts (implemented by
// *notify.Service). Defined here to avoid importing the notify package.
type Notifier interface {
	DeployFailed(ctx context.Context, appID int64, reason string)
}

// Deployer handles deployments through a queue and a worker.
type Deployer struct {
	engine   docker.Engine
	builder  builder.Builder
	store    Store
	hub      *DeployLogHub
	network  string
	notifier Notifier
	queue    chan job
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	mu        sync.Mutex
	cancelJob context.CancelFunc
}

func New(engine docker.Engine, b builder.Builder, store Store, hub *DeployLogHub, network string) *Deployer {
	return &Deployer{
		engine:  engine,
		builder: b,
		store:   store,
		hub:     hub,
		network: network,
		queue:   make(chan job, 64),
		done:    make(chan struct{}),
	}
}

// SetNotifier wires deploy-failure notifications (no-op if never set).
func (d *Deployer) SetNotifier(n Notifier) { d.notifier = n }

func (d *Deployer) Start(ctx context.Context) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.done:
				return
			case j := <-d.queue:
				jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
				d.mu.Lock()
				d.cancelJob = cancel
				d.mu.Unlock()
				d.run(jobCtx, j.deployID, j.noCache)
				cancel()
				d.mu.Lock()
				d.cancelJob = nil
				d.mu.Unlock()
			}
		}
	}()
}

// Enqueue creates a deployment record and puts it in the queue. Returns deployID (0 on error/after Stop).
func (d *Deployer) Enqueue(appID int64, trigger string) int64 {
	return d.enqueue(appID, trigger, false)
}

// EnqueueRebuild is like Enqueue but forces a from-scratch build (docker build
// --no-cache for Dockerfile apps; image apps re-pull as usual).
func (d *Deployer) EnqueueRebuild(appID int64, trigger string) int64 {
	return d.enqueue(appID, trigger, true)
}

func (d *Deployer) enqueue(appID int64, trigger string, noCache bool) int64 {
	select {
	case <-d.done:
		return 0
	default:
	}
	deployID, err := d.store.CreateDeployment(context.Background(), appID, trigger)
	if err != nil {
		slog.Error("create deployment failed", "app", appID, "err", err)
		return 0
	}
	_ = d.store.SetStatus(context.Background(), appID, StatusDeploying)
	d.hub.Open(deployID)
	select {
	case d.queue <- job{deployID: deployID, noCache: noCache}:
	case <-d.done:
		// Server shutting down before the job was queued: mark the deployment
		// failed with a detached context so it isn't orphaned as 'deploying'.
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.store.FinishDeployment(wctx, deployID, "error", "", "server shutting down", "")
		// Leave the app deploying (not error) on shutdown — it never ran; the
		// live status poll reconciles after restart.
		_ = d.store.SetStatus(wctx, appID, StatusDeploying)
	}
	return deployID
}

func (d *Deployer) Stop() {
	d.stopOnce.Do(func() {
		close(d.done)
		d.mu.Lock()
		if d.cancelJob != nil {
			d.cancelJob()
		}
		d.mu.Unlock()
	})
	d.wg.Wait()
}

func (d *Deployer) run(ctx context.Context, deployID int64, noCache bool) {
	app, appErr := d.store.GetDeploymentApp(ctx, deployID)
	out := d.hub.Writer(deployID)
	if appErr != nil {
		d.finish(ctx, deployID, app.ID, StatusError, "", appErr.Error())
		return
	}

	var imageTag string
	var err error
	if app.SourceType == "dockerfile" {
		imageTag = docker.BuildImageTag(app.ID, deployID)
		err = d.builder.Build(ctx, builder.BuildRequest{
			AppID: app.ID, DeployID: deployID,
			GitURL: app.GitURL, GitBranch: app.GitBranch,
			DockerfilePath: app.DockerfilePath, ImageTag: imageTag,
			NoCache: noCache,
		}, out)
	} else {
		imageTag = app.Image + ":" + app.Tag
		fmt.Fprintf(out, "→ deploy image %s\n", imageTag)
	}

	// Baseline task set BEFORE the deploy: convergence below only counts tasks
	// outside it, so the old StartFirst task of a rolling update can never
	// satisfy it (it kept redeploys reporting instant false success).
	var baseline []string
	if err == nil {
		if prev, perr := d.engine.ServiceProgress(ctx, docker.ServiceName(app.ID), nil); perr == nil && prev.Found {
			baseline = prev.TaskIDs
		}
		err = d.engine.ServiceDeploy(ctx, d.buildSpec(app, imageTag))
	}

	if err != nil {
		fmt.Fprintf(out, "❌ %v\n", err)
		slog.Error("deploy failed", "deploy", deployID, "err", err)
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, err.Error())
		return
	}
	fmt.Fprintf(out, "→ waiting for service to converge...\n")
	// A task is not counted Running until its healthcheck passes, and the
	// healthcheck cannot pass before its start_period. Wait at least that long
	// (plus headroom) so a slow-but-healthy app is not falsely failed.
	timeout := convergeTimeout
	if app.Healthcheck != nil && app.Healthcheck.StartPeriod > 0 {
		if want := app.Healthcheck.StartPeriod + 60*time.Second; want > timeout {
			timeout = want
		}
	}
	cctx, ccancel := context.WithTimeout(ctx, timeout)
	defer ccancel()
	converged, crashed, rolledBack := false, false, false
	for {
		st, serr := d.engine.ServiceProgress(cctx, docker.ServiceName(app.ID), baseline)
		// Swarm rolled the update back (FailureAction=Rollback): the new version
		// failed and the old one is being restored — that is a failed deploy, and
		// it must be checked first (the restored old-image task is "new" relative
		// to the baseline and would otherwise read as converged).
		if serr == nil && st.Found && strings.HasPrefix(st.UpdateState, "rollback") {
			rolledBack = true
			break
		}
		if serr == nil && st.Found && st.Desired > 0 && st.Running >= st.Desired {
			converged = true
			break
		}
		// Crash-loop: tasks keep failing. Fail fast instead of waiting the whole
		// timeout (which is for slow starters, not for genuinely broken images).
		if serr == nil && st.Found && st.Failed >= 3 && st.Running < st.Desired {
			crashed = true
			break
		}
		select {
		case <-cctx.Done():
		case <-time.After(convergePollInterval):
			continue
		}
		break
	}
	if converged {
		fmt.Fprintf(out, "✅ deployed %s\n", imageTag)
		d.finish(ctx, deployID, app.ID, StatusRunning, imageTag, "")
		return
	}
	if rolledBack {
		fmt.Fprintf(out, "❌ rolling update failed — swarm rolled back to the previous version\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "update rolled back")
		return
	}
	if crashed {
		fmt.Fprintf(out, "❌ service is crash-looping (tasks keep failing) — check the image, command and env\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "service crash-looping")
		return
	}
	// Not converged within the window. Tell "still starting" and "shutting down"
	// apart from a genuine failure so a slow-but-healthy app is not recorded as
	// an error (the live status poll reconciles it to running).
	if ctx.Err() != nil {
		fmt.Fprintf(out, "⚠ deploy interrupted (server shutting down)\n")
		d.finish(ctx, deployID, app.ID, StatusDeploying, imageTag, "interrupted")
		return
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	st, serr := d.engine.ServiceProgress(fctx, docker.ServiceName(app.ID), baseline)
	fcancel()
	switch {
	case serr == nil && st.Found && strings.HasPrefix(st.UpdateState, "rollback"):
		fmt.Fprintf(out, "❌ rolling update failed — swarm rolled back to the previous version\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "update rolled back")
	case serr == nil && st.Found && st.Desired > 0 && st.Running >= st.Desired:
		fmt.Fprintf(out, "✅ deployed %s\n", imageTag)
		d.finish(ctx, deployID, app.ID, StatusRunning, imageTag, "")
	case serr == nil && st.Found && st.Failed > 0 && st.Running < st.Desired:
		fmt.Fprintf(out, "❌ service has failed tasks (crash-looping) — check the image, command and env\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "service crash-looping")
	case serr == nil && st.Found && (st.Running > 0 || st.UpdateState == "updating"):
		// A new task exists but is not ready yet, or the rolling update is still
		// in flight — leave the app "deploying"; the live poll reconciles it.
		fmt.Fprintf(out, "⚠ service still starting after %s — continuing in the background\n", timeout)
		d.finish(ctx, deployID, app.ID, StatusDeploying, imageTag, "")
	default:
		fmt.Fprintf(out, "❌ service did not become healthy in time\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "service did not converge")
	}
}

// finish closes the log hub, saves the log to deployments, and updates statuses.
func (d *Deployer) finish(ctx context.Context, deployID, appID int64, status, imageTag, errMsg string) {
	full := d.hub.Close(deployID)
	depStatus := "done"
	if status == StatusError {
		depStatus = "error"
	}
	// Use the caller's status directly so a "still starting" deploy can land the
	// app in StatusDeploying (the live /status poll reconciles it to running)
	// instead of being forced to running or error.
	appStatus := status
	// Detached context: persist the final status even if the job ctx was canceled
	// (graceful shutdown), so deployments don't get stuck as running/deploying.
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.store.FinishDeployment(wctx, deployID, depStatus, imageTag, errMsg, full); err != nil {
		slog.Error("finish deployment failed", "deploy", deployID, "err", err)
	}
	if appID != 0 {
		_ = d.store.SetStatus(wctx, appID, appStatus)
	}
	// Detached context: the notifier resolves + sends asynchronously and must
	// survive a canceled job ctx (graceful shutdown right after a failed deploy).
	if status == StatusError && appID != 0 && d.notifier != nil {
		d.notifier.DeployFailed(context.Background(), appID, errMsg)
	}
}

func (d *Deployer) buildSpec(app App, imageTag string) docker.ServiceSpec {
	name := docker.ServiceName(app.ID)
	domains := app.Domains
	if len(domains) == 0 {
		domains = []traefik.Domain{{Host: app.Domain, TLS: false}}
	}
	replicas := app.Replicas
	if replicas == 0 {
		replicas = 1
	}
	return docker.ServiceSpec{
		Name:               name,
		Image:              imageTag,
		Args:               app.Args,
		Env:                app.Env,
		Labels:             traefik.AppLabels(name, domains, app.Port, d.network),
		Replicas:           replicas,
		Network:            d.network,
		RegistryAuth:       app.RegistryAuth,
		MemoryLimitBytes:   app.MemoryLimitBytes,
		NanoCPUs:           app.NanoCPUs,
		RestartCondition:   app.RestartCondition,
		RestartMaxAttempts: app.RestartMaxAttempts,
		Healthcheck:        app.Healthcheck,
	}
}

var _ io.Writer = (*hubWriter)(nil)
