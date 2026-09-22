package deploy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// App — application representation for deployment.
type App struct {
	// MetricsHiddenPaths are metrics paths excluded from every domain.
	MetricsHiddenPaths []string
	ID                 int64
	Name               string
	Image              string
	Tag                string
	Domain             string
	Port               int32
	Env                map[string]string
	SourceType         string // "image" | "dockerfile"
	GitURL             string
	GitBranch          string
	DockerfilePath     string
	Domains            []traefik.Domain
	RegistryAuth       string
	Args               []string // container command override (CMD), e.g. ["start-dev"]

	// Network is the app's organization's overlay network. Empty means the
	// organization has not been migrated yet — the deployer falls back to its
	// configured network.
	Network string

	// ContainerLabels identify the app to the observability agent's log
	// collector (see observability.ContainerLabels).
	ContainerLabels map[string]string

	Replicas           uint64
	MemoryLimitBytes   int64
	NanoCPUs           int64
	RestartCondition   string
	RestartMaxAttempts uint64
	Healthcheck        *docker.HealthcheckSpec
	Mounts             []docker.MountSpec
	Ports              []docker.PortSpec // raw host-published L4 ports (host mode), bypassing Traefik

	GitAuth      *builder.GitAuth  // private-repo HTTPS credentials (dockerfile source)
	BuildArgs    map[string]string // --build-arg (non-secret)
	BuildSecrets map[string]string // BuildKit --secret

	PlacementMode  string   // "any" | "pin" | "global"
	PlacementNodes []string // swarm node IDs the app may run on (for pin/global)
}

// Store — what the deployer needs from the store.
type Store interface {
	GetApplication(ctx context.Context, id int64) (App, error)
	// GetDeploymentApp loads the app a deployment deploys. A non-zero ref is
	// the image reference the job carries; it replaces the row's for an image
	// app, including in the registry-credential host check.
	GetDeploymentApp(ctx context.Context, deployID int64, ref ImageRef) (App, error)
	SetStatus(ctx context.Context, id int64, status string) error
	CreateDeployment(ctx context.Context, appID int64, trigger string) (int64, error)
	CountRunningDeployments(ctx context.Context, appID int64) (int64, error)
	CountRunningDeploymentsByOrg(ctx context.Context, appID int64) (int64, error)
	// SetApplicationImage records the image reference an accepted deploy
	// carries, so the application row shows it and later deploys reuse it.
	SetApplicationImage(ctx context.Context, appID int64, image, tag string) error
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

// ImageRef is an image app's image and tag. The zero value means "whatever the
// application row holds".
type ImageRef struct {
	Image string
	Tag   string
}

// job is one queued unit of work: a deployment plus build options.
type job struct {
	deployID int64
	appID    int64
	noCache  bool // force docker build --no-cache (Rebuild)

	// image and tag, when set, are the image reference this deploy ships
	// (image apps only). They travel with the job rather than through the
	// application row: the worker picks a job up later, and by then the row
	// may carry another request's tag, which this deploy would then ship.
	image string
	tag   string
}

// Notifier is the optional sink for deploy-failure alerts (implemented by
// *notify.Service). Defined here to avoid importing the notify package.
type Notifier interface {
	DeployFailed(ctx context.Context, appID int64, reason string)
}

// Deployer handles deployments through a queue and a worker.
type Deployer struct {
	engine    docker.Engine
	builder   builder.Builder
	store     Store
	hub       *DeployLogHub
	network   string
	notifier  Notifier
	queue     chan job
	done      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	runCancel context.CancelFunc // cancels the in-flight job's context on Stop

	defaultMemBytes int64 // instance-wide default memory limit, 0 = none
	defaultNanoCPUs int64 // instance-wide default CPU limit, 0 = none

	maxBuildsPerOrg int // cap on in-flight deploys per organization, 0 = no cap

	// admitMu makes enqueue's count-then-create one step: without it two
	// overlapping requests for one app (a double click, a retrying agent) both
	// count zero in flight and both queue a deploy.
	admitMu sync.Mutex
}

// SetMaxBuildsPerOrg wires the per-organization in-flight-deploy cap (wired
// from config at startup). <= 0 means no cap: the per-org check in enqueue is
// skipped entirely rather than merely being satisfied by a large number.
func (d *Deployer) SetMaxBuildsPerOrg(n int) {
	d.maxBuildsPerOrg = n
}

// SetResourceDefaults wires the instance-wide memory/CPU limits applied to any
// app that has no explicit value of its own (wired from config at startup).
// 0 means no default for that resource.
func (d *Deployer) SetResourceDefaults(memBytes, nanoCPUs int64) {
	d.defaultMemBytes = memBytes
	d.defaultNanoCPUs = nanoCPUs
}

// withResourceDefaults fills in the instance-wide limits for an app that has
// none. An unlimited container can starve every other tenant on the host, so
// "no limit" is not a safe default; an explicit per-app value always wins.
func (d *Deployer) withResourceDefaults(a App) App {
	if a.MemoryLimitBytes == 0 {
		a.MemoryLimitBytes = d.defaultMemBytes
	}
	if a.NanoCPUs == 0 {
		a.NanoCPUs = d.defaultNanoCPUs
	}
	return a
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
	// runCtx is canceled by Stop so the in-flight job's context unblocks instead
	// of running to its full timeout during graceful shutdown.
	runCtx, runCancel := context.WithCancel(ctx)
	d.runCancel = runCancel
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.done:
				d.drainQueue()
				return
			case j := <-d.queue:
				// select picks randomly when both channels are ready, so a job can
				// be dequeued exactly as we stop — re-check before running it.
				select {
				case <-d.done:
					d.failQueued(j, "server shutting down")
					d.drainQueue()
					return
				default:
				}
				jobCtx, cancel := context.WithTimeout(runCtx, jobTimeout)
				d.run(jobCtx, j)
				cancel()
			}
		}
	}()
}

// drainQueue fails every job still sitting in the queue at shutdown so its
// deployment row isn't orphaned as 'running' forever.
func (d *Deployer) drainQueue() {
	for {
		select {
		case j := <-d.queue:
			d.failQueued(j, "server shutting down")
		default:
			return
		}
	}
}

// failQueued marks a never-run queued job's deployment failed (detached ctx) and
// closes its log feed. App status is left "deploying" — the live status poll
// reconciles it after restart, matching enqueue's shutdown branch.
func (d *Deployer) failQueued(j job, reason string) {
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = d.store.FinishDeployment(wctx, j.deployID, "error", "", reason, "")
	d.hub.Close(j.deployID)
}

// Enqueue creates a deployment record and puts it in the queue. Returns deployID (0 on error/after Stop).
func (d *Deployer) Enqueue(appID int64, trigger string) int64 {
	return d.enqueue(appID, trigger, job{})
}

// EnqueueImage is Enqueue for an image app moving to image:tag. The reference
// is carried in the job, so this deploy ships exactly it whatever the
// application row says by the time the worker gets to it, and it is written to
// the row only once the job has been accepted: a refused deploy leaves the
// application's image untouched.
func (d *Deployer) EnqueueImage(appID int64, trigger, image, tag string) int64 {
	return d.enqueue(appID, trigger, job{image: image, tag: tag})
}

// EnqueueRebuild is like Enqueue but forces a from-scratch build (docker build
// --no-cache for Dockerfile apps; image apps re-pull as usual).
func (d *Deployer) EnqueueRebuild(appID int64, trigger string) int64 {
	return d.enqueue(appID, trigger, job{noCache: true})
}

// systemQueueRetry is how long EnqueueSystem waits between attempts while the
// shared queue is full. One worker drains it, so retrying sooner only burns CPU.
const systemQueueRetry = 2 * time.Second

// EnqueueSystem queues a deploy the control plane submits for itself, skipping
// both the per-app in-flight guard and the per-organization cap. Those caps
// exist to stop one tenant from monopolising the single shared build worker;
// the startup migration onto per-organization networks is not a tenant, and has
// to move every service of an organization in one pass — the per-org cap would
// silently refuse it from the third app onward and leave the rest of the
// organization stranded on the old network. Nothing but that migration may use
// this.
//
// It also waits for room rather than failing when the queue is full: an
// organization with more applications than the queue is deep would otherwise
// lose its tail, and a dropped system deploy strands that service on the shared
// network with nothing to retry it. ctx bounds the wait.
func (d *Deployer) EnqueueSystem(ctx context.Context, appID int64) int64 {
	for {
		select {
		case <-d.done:
			return 0
		case <-ctx.Done():
			slog.Warn("system deploy gave up waiting for a queue slot", "app", appID)
			return 0
		default:
		}
		if !d.queueFull() {
			if id := d.submit(appID, TriggerSystem, job{}); id != 0 {
				return id
			}
			// submit only fails for a full queue (lost the race for the slot we
			// just saw free) or a store error. A store error will not fix itself.
			if !d.queueFull() {
				return 0
			}
		}
		select {
		case <-d.done:
			return 0
		case <-ctx.Done():
			slog.Warn("system deploy gave up waiting for a queue slot", "app", appID)
			return 0
		case <-time.After(systemQueueRetry):
		}
	}
}

// queueFull reports whether the shared job queue has no room left.
func (d *Deployer) queueFull() bool { return len(d.queue) >= cap(d.queue) }

func (d *Deployer) enqueue(appID int64, trigger string, opts job) int64 {
	select {
	case <-d.done:
		return 0
	default:
	}
	d.admitMu.Lock()
	defer d.admitMu.Unlock()
	// Refuse a second deploy for an app that already has one in flight. The
	// queue is 64 deep, drained by a single worker and shared by every tenant,
	// so a caller retrying the same app — an agent in a retry loop is the
	// obvious way this happens — could fill it and make every other tenant's
	// deploys fail with "queue full". Bounding each app to one pending deploy
	// caps that at one slot per app.
	if n, err := d.store.CountRunningDeployments(context.Background(), appID); err != nil {
		slog.Error("in-flight deploy check failed", "app", appID, "err", err)
		return 0
	} else if n > 0 {
		slog.Info("deploy rejected, one is already in flight", "app", appID, "in_flight", n)
		return 0
	}
	// Cap in-flight deploys per organization. Without this, one tenant with many
	// apps can hold the single shared build worker indefinitely and stall every
	// other organization's deploys. 0 (the default when unset) means no cap, and
	// skips the query entirely rather than relying on a huge threshold.
	if d.maxBuildsPerOrg > 0 {
		n, err := d.store.CountRunningDeploymentsByOrg(context.Background(), appID)
		if err != nil {
			slog.Error("per-org in-flight check failed", "app", appID, "err", err)
			return 0
		}
		if n >= int64(d.maxBuildsPerOrg) {
			slog.Info("deploy rejected, the organization is at its in-flight limit", "app", appID, "in_flight", n)
			return 0
		}
	}
	return d.submit(appID, trigger, opts)
}

// submit creates the deployment row and hands the job to the worker. It is the
// part of enqueue that both the capped (tenant) path and EnqueueSystem share.
// opts carries the build options and image reference; its ids are filled here.
func (d *Deployer) submit(appID int64, trigger string, opts job) int64 {
	deployID, err := d.store.CreateDeployment(context.Background(), appID, trigger)
	if err != nil {
		slog.Error("create deployment failed", "app", appID, "err", err)
		return 0
	}
	_ = d.store.SetStatus(context.Background(), appID, StatusDeploying)
	d.hub.Open(deployID)
	select {
	case d.queue <- job{deployID: deployID, appID: appID, noCache: opts.noCache, image: opts.image, tag: opts.tag}:
		if opts.tag != "" {
			// After the send, not before: a refused job must not leave its tag
			// on the application. The worker never reads it from the row, so
			// writing it now cannot change what this deploy ships. Should the
			// write fail, the deploy still ships the tag it carries; only the
			// next deploy without one falls back to the old tag.
			wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.store.SetApplicationImage(wctx, appID, opts.image, opts.tag); err != nil {
				slog.Error("record deployed image on the application failed", "app", appID, "deploy", deployID, "image", opts.image, "tag", opts.tag, "err", err)
			}
		}
	case <-d.done:
		// Server shutting down before the job was queued: mark the deployment
		// failed with a detached context so it isn't orphaned as 'deploying'.
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.store.FinishDeployment(wctx, deployID, "error", "", "server shutting down", "")
		// Leave the app deploying (not error) on shutdown — it never ran; the
		// live status poll reconciles after restart.
		_ = d.store.SetStatus(wctx, appID, StatusDeploying)
	default:
		// Queue full (a full queue is hours of backlog with one worker): fail the
		// deployment immediately instead of blocking the HTTP handler goroutine
		// until a slot frees up.
		slog.Warn("deploy queue full, rejecting deployment", "app", appID, "deploy", deployID)
		d.hub.Close(deployID)
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.store.FinishDeployment(wctx, deployID, "error", "", "deploy queue full, try again later", "")
		_ = d.store.SetStatus(wctx, appID, StatusError)
		return 0
	}
	return deployID
}

func (d *Deployer) Stop() {
	d.stopOnce.Do(func() {
		close(d.done)
		if d.runCancel != nil {
			d.runCancel() // unblock any in-flight job ctx
		}
	})
	d.wg.Wait()
}

func (d *Deployer) run(ctx context.Context, j job) {
	deployID, noCache := j.deployID, j.noCache
	app, appErr := d.store.GetDeploymentApp(ctx, deployID, ImageRef{Image: j.image, Tag: j.tag})
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
			NoCache:      noCache,
			GitAuth:      app.GitAuth,
			BuildArgs:    app.BuildArgs,
			BuildSecrets: app.BuildSecrets,
		}, out)
	} else {
		imageTag = app.Image + ":" + app.Tag
		imageTag = resolveImageRef(ctx, d.engine, app, imageTag)
		fmt.Fprintf(out, "→ deploy image %s\n", imageTag)
	}

	// Chown owned volumes to the app's runtime uid:gid before the task starts
	// (fresh named volumes are root:root; a non-root app can't write). Runs on
	// the app's placement node(s); a failure fails the deploy.
	if err == nil {
		err = d.chownOwnedVolumes(ctx, app, out)
	}

	// Baseline task set BEFORE the deploy: convergence below only counts tasks
	// outside it, so the old StartFirst task of a rolling update can never
	// satisfy it (it kept redeploys reporting instant false success).
	var baseline []string
	if err == nil {
		// A snapshot we cannot take is not an empty snapshot: an empty baseline
		// makes the old StartFirst task count as new, so convergence would be
		// satisfied instantly by the version we are replacing. Fail the deploy
		// rather than report a success nobody verified. (prev.Found == false is
		// legitimate — the service does not exist yet on a first deploy.)
		prev, perr := d.engine.ServiceProgress(ctx, docker.ServiceName(app.ID), nil)
		if perr != nil {
			err = fmt.Errorf("snapshot running tasks before deploy: %w", perr)
		} else {
			if prev.Found {
				baseline = prev.TaskIDs
			}
			err = d.engine.ServiceDeploy(ctx, d.buildSpec(app, imageTag))
		}
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
	firstPoll := true
	for {
		st, serr := d.engine.ServiceProgress(cctx, docker.ServiceName(app.ID), baseline)
		// Swarm rolled the update back (FailureAction=Rollback): the new version
		// failed and the old one is being restored — that is a failed deploy, and
		// it must be checked first (the restored old-image task is "new" relative
		// to the baseline and would otherwise read as converged).
		//
		// Skip this on the very first poll: Swarm persists the previous deploy's
		// UpdateStatus, and ServiceUpdate returning does not guarantee the manager
		// has transitioned it for THIS update yet — an immediate read could see a
		// stale "rollback_*" from a prior failed deploy. A genuine rollback of the
		// current update only appears after its tasks fail (seconds later), well
		// past the first poll; the post-timeout check below is the backstop.
		if serr == nil && st.Found && !firstPoll && strings.HasPrefix(st.UpdateState, "rollback") {
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
		firstPoll = false
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
	app = d.withResourceDefaults(app)
	name := docker.ServiceName(app.ID)
	domains := app.Domains
	if len(domains) == 0 {
		domains = []traefik.Domain{{Host: app.Domain, TLS: false}}
	}
	replicas := app.Replicas
	if replicas == 0 {
		replicas = 1
	}
	// An app deploys into its organization's network; the configured network is
	// only the fallback for an installation that has not been migrated yet.
	net := app.Network
	if net == "" {
		net = d.network
	}
	spec := docker.ServiceSpec{
		Name:               name,
		Image:              imageTag,
		Args:               app.Args,
		Env:                app.Env,
		Labels:             traefik.AppLabels(name, domains, app.Port, net, app.MetricsHiddenPaths),
		ContainerLabels:    app.ContainerLabels,
		Replicas:           replicas,
		Network:            net,
		RegistryAuth:       app.RegistryAuth,
		MemoryLimitBytes:   app.MemoryLimitBytes,
		NanoCPUs:           app.NanoCPUs,
		RestartCondition:   app.RestartCondition,
		RestartMaxAttempts: app.RestartMaxAttempts,
		Healthcheck:        app.Healthcheck,
		Mounts:             app.Mounts,
		Ports:              app.Ports,
	}
	// Placement: restrict to the chosen node set via the per-app krill.place label
	// (set on those nodes by the server). pin spreads replicas across them; global
	// runs one task per node in the set. "any"/empty → default cluster-wide spread.
	if len(app.PlacementNodes) > 0 {
		switch app.PlacementMode {
		case "pin":
			spec.Constraints = append(spec.Constraints, fmt.Sprintf("node.labels.krill.place.%d==1", app.ID))
			spec.SpreadNodeID = true
		case "global":
			spec.Constraints = append(spec.Constraints, fmt.Sprintf("node.labels.krill.place.%d==1", app.ID))
			spec.Global = true
		}
	}
	return spec
}

// chownNodes returns the swarm node IDs on which owned volumes must be chowned
// before deploy: the app's pinned/global node set, or [""] (the local/control-
// plane daemon) otherwise. "" is resolved to the local client by selectClient.
func chownNodes(app App) []string {
	if len(app.PlacementNodes) > 0 && (app.PlacementMode == "pin" || app.PlacementMode == "global") {
		return app.PlacementNodes
	}
	return []string{""}
}

// splitOwner parses a normalized "uid:gid" (as stored by volume.ParseOwner).
func splitOwner(s string) (uid, gid int, ok bool) {
	a, b, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	ui, e1 := strconv.Atoi(a)
	gi, e2 := strconv.Atoi(b)
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return ui, gi, true
}

// chownOwnedVolumes runs the pre-start chown sidecar for every mount that has an
// Owner set, on each node the app is placed on. Any failure fails the deploy —
// chowning before the task starts is what makes a non-root app's volume writable
// and avoids the crash-loop → rollback cycle.
func (d *Deployer) chownOwnedVolumes(ctx context.Context, app App, out io.Writer) error {
	nodes := chownNodes(app)
	for _, m := range app.Mounts {
		if m.Owner == "" {
			continue
		}
		uid, gid, ok := splitOwner(m.Owner)
		if !ok {
			return fmt.Errorf("volume %s: malformed owner %q", m.Source, m.Owner)
		}
		for _, node := range nodes {
			fmt.Fprintf(out, "→ preparing volume ownership (%s → %d:%d)\n", m.Source, uid, gid)
			if err := d.engine.VolumeChown(ctx, m.Source, uid, gid, node); err != nil {
				return fmt.Errorf("chown volume %s: %w", m.Source, err)
			}
		}
	}
	return nil
}

// resolveImageRef pins an image app's tag to its current registry digest so a
// same-tag redeploy actually pulls the new image. Best-effort: on any error it
// returns the plain tag (Swarm still deploys, just without forced re-pull).
func resolveImageRef(ctx context.Context, engine docker.Engine, app App, imageTag string) string {
	pinned, err := engine.ResolveDigest(ctx, imageTag, app.RegistryAuth)
	if err == nil && pinned != "" {
		return pinned
	}
	if err != nil {
		slog.Warn("digest resolve failed, deploying plain tag", "image", imageTag, "err", err)
	}
	return imageTag
}

var _ io.Writer = (*hubWriter)(nil)
