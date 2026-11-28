package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/appmetrics"
	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/traefik"
)

// App — application representation for deployment.
type App struct {
	RuntimePinned    bool
	MetricsEndpoints []appmetrics.Endpoint
	Runtime          RuntimeSettings
	Dependencies     string
	// MetricsHiddenPaths are metrics paths excluded from every domain.
	MetricsHiddenPaths []string
	ID                 int64
	PredeployCommand   string
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

	// MetricsTokenEnv names the env variable that carries the metrics token,
	// "" when none is injected.
	MetricsTokenEnv string

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

	// LinkEgress is the Krill link selection a deploy applies, sorted
	// "host:port": the saved one while the installation is a link client and
	// the app's organization is granted, empty otherwise. Non-empty puts the
	// app on its own egress network as a second network.
	LinkEgress []string
}

// EgressHook is the Krill link's part in a deploy. Prepare runs before an app
// with a selection deploys onto its egress network: it creates the network
// and has the egress sidecar answer for the app's hosts there, and returns
// once the sidecar does — a deploy it fails is failed, since the app would
// otherwise resolve those hosts to their real addresses. Changed is told,
// without waiting, that a deploy of the app finished: the sidecar may then
// drop a host the app no longer applies.
type EgressHook interface {
	Prepare(ctx context.Context, appID int64, dests []string) error
	Changed(appID int64)
}

// ErrEgressUnavailable fails a deploy that has a link selection to apply
// while the link is not wired.
var ErrEgressUnavailable = errors.New("the Krill link is not available to apply this app's egress destinations")

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

	// settleTimeout is how long a deploy still starting after convergeTimeout
	// keeps being watched in the background (a large image pull can take
	// minutes) before it is recorded as a convergence timeout. The build
	// worker is not held meanwhile.
	settleTimeout      = 20 * time.Minute
	settlePollInterval = 5 * time.Second
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
	SkipBuild       bool
	CredentialImage string
	Image           string
	Tag             string
}

// job is one queued unit of work: a deployment plus build options.
type job struct {
	deployID         int64
	appID            int64
	skipPredeploy    bool
	rollbackID       int64
	rollbackExpected *RuntimeSettings
	noCache          bool // force docker build --no-cache (Rebuild)
	system           bool // EnqueueSystem: the network migration moving the app

	// applyImage, when set, makes this an "apply settings" deploy: the
	// current settings on this content-addressed image, which deployment
	// applyFrom runs. Nothing is pulled by tag or built. applySource is the
	// source-section digest it was accepted for; the worker refuses the
	// deploy when the image or build settings changed meanwhile.
	applyImage  string
	applyFrom   int64
	applySource string

	// image and tag, when set, are the image reference this deploy ships
	// (image apps only). They travel with the job rather than through the
	// application row: the worker picks a job up later, and by then the row
	// may carry another request's tag, which this deploy would then ship.
	image string
	tag   string
}

// Notifier is the optional sink for deploy alerts (implemented by
// *notify.Service). Defined here to avoid importing the notify package.
type Notifier interface {
	DeployFailed(ctx context.Context, appID int64, reason string)
	// DeploySucceeded is told about a deploy that finished with the app
	// running (not one still starting in the background).
	DeploySucceeded(ctx context.Context, deploymentID int64)
}

// Deployer handles deployments through a queue and a worker.
type Deployer struct {
	engine    docker.Engine
	builder   builder.Builder
	store     Store
	hub       *DeployLogHub
	network   string
	notifier  Notifier
	queue     *jobQueue
	done      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	runCancel context.CancelFunc // cancels the in-flight job's context on Stop
	runCtx    context.Context    // parent of background settles; canceled by Stop

	defaultMemBytes int64 // instance-wide default memory limit, 0 = none
	defaultNanoCPUs int64 // instance-wide default CPU limit, 0 = none

	maxBuildsPerOrg int // cap on in-flight deploys per organization, 0 = no cap

	// settling tracks the deploys being watched in the background, one per
	// app (the per-app in-flight check admits no second tenant deploy; only a
	// system deploy can start meanwhile, and it supersedes the watch).
	settleMu sync.Mutex
	settling map[int64]*settleWatch

	// admitMu makes enqueue's count-then-create one step: without it two
	// overlapping requests for one app (a double click, a retrying agent) both
	// count zero in flight and both queue a deploy.
	admitMu sync.Mutex

	// appStops counts the Stops under way per app (under admitMu): an apply
	// is not admitted while one runs, as it could pass its checks before the
	// Stop scales the service and run its predeploy command meanwhile.
	appStops map[int64]int

	// applied holds what a successful deploy applied from complete until
	// finishAs writes it with the done status.
	appliedMu sync.Mutex
	applied   map[int64]AppliedConfig

	// tracked holds every accepted deploy, by deployment id, until its outcome
	// is recorded: what StopApp needs to cancel the ones that have not applied
	// their service spec yet (see stop.go).
	trackMu sync.Mutex
	tracked map[int64]*tracked

	// egress is the Krill link's deploy hook, nil when not wired.
	egress EgressHook
}

// SetEgress wires the Krill link's deploy hook. Call it before Start.
func (d *Deployer) SetEgress(h EgressHook) { d.egress = h }

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
	if a.RuntimePinned {
		return a
	}
	if a.MemoryLimitBytes == 0 {
		a.MemoryLimitBytes = d.defaultMemBytes
	}
	if a.NanoCPUs == 0 {
		a.NanoCPUs = d.defaultNanoCPUs
	}
	return a
}

func New(engine docker.Engine, b builder.Builder, store Store, hub *DeployLogHub, network string) *Deployer {
	// Background watches need a context Stop cancels even when run is driven
	// without Start (tests); Start replaces it with one derived from its ctx.
	runCtx, runCancel := context.WithCancel(context.Background())
	return &Deployer{
		engine:    engine,
		builder:   b,
		store:     store,
		hub:       hub,
		network:   network,
		queue:     newJobQueue(queueCapacity),
		done:      make(chan struct{}),
		runCtx:    runCtx,
		runCancel: runCancel,
	}
}

// SetNotifier wires deploy notifications (no-op if never set).
func (d *Deployer) SetNotifier(n Notifier) { d.notifier = n }

func (d *Deployer) Start(ctx context.Context) {
	// runCtx is canceled by Stop so the in-flight job's context unblocks instead
	// of running to its full timeout during graceful shutdown.
	if d.runCancel != nil {
		d.runCancel()
	}
	runCtx, runCancel := context.WithCancel(ctx)
	d.runCancel = runCancel
	d.runCtx = runCtx
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			if d.stopping() {
				d.drainQueue()
				return
			}
			j, ok := d.queue.pop()
			if !ok {
				select {
				case <-d.done:
				case <-d.queue.wake:
				}
				continue
			}
			jobCtx, cancel := context.WithTimeout(runCtx, jobTimeout)
			d.run(jobCtx, j)
			cancel()
		}
	}()
}

// drainQueue fails every job still sitting in the queue at shutdown so its
// deployment row isn't orphaned as 'running' forever.
func (d *Deployer) drainQueue() {
	for _, j := range d.queue.takeAll() {
		d.failQueued(j, "server shutting down")
	}
}

// failQueued marks a never-run queued job's deployment failed (detached ctx) and
// closes its log feed. App status is left "deploying" — StartStatusHeal sets
// it from the service after restart, matching enqueue's shutdown branch.
func (d *Deployer) failQueued(j job, reason string) {
	if !d.claimFinish(j.deployID) {
		return // a stop cancelled it and records it
	}
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = d.store.FinishDeployment(wctx, j.deployID, "error", "", reason, "")
	d.hub.Close(j.deployID)
}

// Why a deploy was not accepted. A refused deploy creates no deployment row
// and changes no application state, so it never shows up as pending.
var (
	ErrDeployInFlight  = errors.New("a deployment of this application is already in progress")
	ErrOrgDeployLimit  = errors.New("the organization has reached its limit of deployments in progress")
	ErrQueueFull       = errors.New("the deploy queue is full")
	ErrDeployerStopped = errors.New("Krill is shutting down")
)

// Enqueue creates a deployment record and puts it in the queue. Returns deployID (0 on error/after Stop).
func (d *Deployer) Enqueue(appID int64, trigger string) int64 {
	id, _ := d.enqueue(appID, trigger, job{})
	return id
}

// EnqueueErr is Enqueue reporting why a deploy was refused: one of the Err*
// values above, or a wrapped store error.
func (d *Deployer) EnqueueErr(appID int64, trigger string) (int64, error) {
	return d.enqueue(appID, trigger, job{})
}

// EnqueueImageErr is EnqueueImage reporting why a deploy was refused.
func (d *Deployer) EnqueueImageErr(appID int64, trigger, image, tag string) (int64, error) {
	return d.enqueue(appID, trigger, job{image: image, tag: tag})
}

// EnqueueRebuildErr is EnqueueRebuild reporting why a deploy was refused.
func (d *Deployer) EnqueueRebuildErr(appID int64, trigger string) (int64, error) {
	return d.enqueue(appID, trigger, job{noCache: true})
}

// EnqueueImage is Enqueue for an image app moving to image:tag. The reference
// is carried in the job, so this deploy ships exactly it whatever the
// application row says by the time the worker gets to it, and it is written to
// the row only once the job has been accepted: a refused deploy leaves the
// application's image untouched.
func (d *Deployer) EnqueueImage(appID int64, trigger, image, tag string) int64 {
	id, _ := d.enqueue(appID, trigger, job{image: image, tag: tag})
	return id
}

// EnqueueRebuild is like Enqueue but forces a from-scratch build (docker build
// --no-cache for Dockerfile apps; image apps re-pull as usual).
func (d *Deployer) EnqueueRebuild(appID int64, trigger string) int64 {
	id, _ := d.enqueue(appID, trigger, job{noCache: true})
	return id
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
		// Under admitMu like every tenant enqueue: then the queue cannot fill
		// between a check and a send, and enqueue's own full-queue check
		// (which refuses before writing a deployment row) stays exact.
		d.admitMu.Lock()
		full := d.queueFull()
		var id int64
		if !full {
			id, _ = d.submit(appID, TriggerSystem, job{skipPredeploy: true, system: true})
		}
		d.admitMu.Unlock()
		if id != 0 {
			return id
		}
		if !full {
			// The queue had room, so submit failed on the store; that will
			// not fix itself.
			return 0
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
func (d *Deployer) queueFull() bool { return d.queue.full() }

func (d *Deployer) enqueue(appID int64, trigger string, opts job) (int64, error) {
	select {
	case <-d.done:
		return 0, ErrDeployerStopped
	default:
	}
	d.admitMu.Lock()
	defer d.admitMu.Unlock()
	if opts.applyImage != "" && d.appStops[appID] > 0 {
		return 0, ErrApplyStopped
	}
	// Refuse a second deploy for an app that already has one in flight. The
	// queue is 64 deep, drained by a single worker and shared by every tenant,
	// so a caller retrying the same app — an agent in a retry loop is the
	// obvious way this happens — could fill it and make every other tenant's
	// deploys fail with "queue full". Bounding each app to one pending deploy
	// caps that at one slot per app.
	if n, err := d.store.CountRunningDeployments(context.Background(), appID); err != nil {
		slog.Error("in-flight deploy check failed", "app", appID, "err", err)
		return 0, fmt.Errorf("check deployments in progress: %w", err)
	} else if n > 0 {
		slog.Info("deploy rejected, one is already in flight", "app", appID, "in_flight", n)
		return 0, ErrDeployInFlight
	}
	// Cap in-flight deploys per organization. Without this, one tenant with many
	// apps can hold the single shared build worker indefinitely and stall every
	// other organization's deploys. 0 (the default when unset) means no cap, and
	// skips the query entirely rather than relying on a huge threshold.
	if d.maxBuildsPerOrg > 0 {
		n, err := d.store.CountRunningDeploymentsByOrg(context.Background(), appID)
		if err != nil {
			slog.Error("per-org in-flight check failed", "app", appID, "err", err)
			return 0, fmt.Errorf("check the organization's deployments in progress: %w", err)
		}
		if n >= int64(d.maxBuildsPerOrg) {
			slog.Info("deploy rejected, the organization is at its in-flight limit", "app", appID, "in_flight", n)
			return 0, ErrOrgDeployLimit
		}
	}
	// Every sender (enqueue and EnqueueSystem) adds to the queue only while
	// holding admitMu, so a queue seen with room here still has it when
	// submit sends: refuse before a deployment row exists, instead of
	// recording a failed deployment and marking an app that may be running
	// fine as errored.
	if d.queueFull() {
		slog.Warn("deploy rejected, the queue is full", "app", appID)
		return 0, ErrQueueFull
	}
	return d.submit(appID, trigger, opts)
}

// submit creates the deployment row and hands the job to the worker. It is the
// part of enqueue that both the capped (tenant) path and EnqueueSystem share.
// opts carries the build options and image reference; its ids are filled here.
func (d *Deployer) submit(appID int64, trigger string, opts job) (int64, error) {
	deployID, err := d.store.CreateDeployment(context.Background(), appID, trigger)
	if err != nil {
		slog.Error("create deployment failed", "app", appID, "err", err)
		return 0, fmt.Errorf("create deployment: %w", err)
	}
	if opts.rollbackID != 0 {
		if err := d.store.(releaseStore).RecordRollbackSource(context.Background(), deployID, opts.rollbackID); err != nil {
			d.store.FinishDeployment(context.Background(), deployID, "error", "", "could not record rollback source", "")
			return 0, fmt.Errorf("record rollback source: %w", err)
		}
	}
	_ = d.store.SetStatus(context.Background(), appID, StatusDeploying)
	d.hub.Open(deployID)
	// Tracked before the send: the worker may pick the job up at once, and a
	// stop must find it either way.
	d.track(deployID, appID)
	if d.stopping() {
		// Server shutting down before the job was queued: mark the deployment
		// failed with a detached context so it isn't orphaned as 'deploying'.
		if !d.claimFinish(deployID) {
			return 0, ErrDeployerStopped // a stop cancelled it and records it
		}
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.store.FinishDeployment(wctx, deployID, "error", "", "server shutting down", "")
		// Leave the app deploying (not error) on shutdown — it never ran;
		// StartStatusHeal sets it from the service after restart.
		_ = d.store.SetStatus(wctx, appID, StatusDeploying)
		return 0, ErrDeployerStopped
	}
	queued := opts
	queued.deployID, queued.appID = deployID, appID
	if !d.queue.push(queued) {
		// Queue full (a full queue is hours of backlog with one worker): fail the
		// deployment immediately instead of blocking the HTTP handler goroutine
		// until a slot frees up.
		slog.Warn("deploy queue full, rejecting deployment", "app", appID, "deploy", deployID)
		d.claimFinish(deployID) // under admitMu: no stop can have cancelled it
		d.hub.Close(deployID)
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.store.FinishDeployment(wctx, deployID, "error", "", "deploy queue full, try again later", "")
		_ = d.store.SetStatus(wctx, appID, StatusError)
		return 0, ErrQueueFull
	}
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
	return deployID, nil
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
	// A stop of the app cancels this context while the deploy has not applied
	// its service spec yet (see StopApp).
	ctx, cancelJob := context.WithCancelCause(ctx)
	defer cancelJob(nil)
	if !d.begin(deployID, cancelJob) {
		slog.Info("deploy dropped, the app was stopped while it was queued", "deploy", deployID, "app", j.appID)
		return
	}
	// A deploy its stop recorded as cancelled is left without a word more.
	defer d.abandon(deployID)
	var app App
	var err error
	var imageTag string
	var snapshot releaseSnapshot
	var expected *RuntimeSettings
	releaseSaved := false
	rs, releaseCapable := d.store.(releaseStore)
	out := d.hub.Writer(deployID)
	if j.rollbackID != 0 {
		if !releaseCapable {
			err = ErrReleaseUnavailable
		} else {
			snapshot, err = rs.LoadRelease(ctx, j.appID, j.rollbackID)
			if err == nil {
				app, err = rs.RollbackApplication(ctx, j.appID, snapshot.Image)
			}
			if err == nil && (j.rollbackExpected == nil || !reflect.DeepEqual(app.Runtime, *j.rollbackExpected) || !reflect.DeepEqual(guards(app), snapshot.Guards)) {
				err = ErrReleaseDrift
			}
			if err == nil {
				original := app.Runtime
				expected = &original
				app = applyRelease(app, snapshot)
				imageTag = snapshot.Image
				if validator, ok := d.engine.(docker.ReleaseValidator); ok {
					err = validator.ValidateRelease(ctx, d.buildSpec(app, imageTag), app.PlacementNodes)
				} else {
					err = ErrReleaseUnavailable
				}
			}
		}
	} else if j.applyImage != "" {
		// Loaded with the build settings (not SkipBuild), so its source
		// fingerprint is comparable; the registry credential is checked
		// against the pinned image actually deployed.
		app, err = d.store.GetDeploymentApp(ctx, deployID, ImageRef{CredentialImage: j.applyImage})
		if err == nil && d.configFingerprint(app).Sections[SectionSource] != j.applySource {
			err = applySkip{ErrApplyNeedsDeploy}
		}
		imageTag = j.applyImage
	} else {
		app, err = d.store.GetDeploymentApp(ctx, deployID, ImageRef{Image: j.image, Tag: j.tag})
	}
	if err != nil && j.applyImage != "" && errors.As(err, new(applySkip)) {
		fmt.Fprintf(out, "⏹ %v\n", err)
		d.finishApplySkipped(deployID, j.appID, j.applyImage, err.Error())
		return
	}
	if err != nil {
		message := err.Error()
		if j.rollbackID != 0 {
			message = ErrReleaseDrift.Error()
		}
		fmt.Fprintln(out, message)
		d.finish(ctx, deployID, j.appID, StatusError, "", message)
		return
	}

	// version is what notifications and the digest call this deploy (see
	// imageVersion and gitVersion); recorded once the image is known.
	var version string
	if j.rollbackID != 0 {
		fmt.Fprintf(out, "→ rollback to deployment %d (%s)\n", j.rollbackID, imageTag)
		version = d.sourceVersion(ctx, j.rollbackID)
	} else if j.applyImage != "" {
		fmt.Fprintf(out, "→ apply settings on image %s of deployment %d (no pull of a tag, no build)\n", imageTag, j.applyFrom)
		version = d.sourceVersion(ctx, j.applyFrom)
	} else if app.SourceType == "dockerfile" {
		imageTag = docker.BuildImageTag(app.ID, deployID)
		var res builder.BuildResult
		res, err = d.builder.Build(ctx, builder.BuildRequest{
			AppID: app.ID, DeployID: deployID,
			GitURL: app.GitURL, GitBranch: app.GitBranch,
			DockerfilePath: app.DockerfilePath, ImageTag: imageTag,
			NoCache:      noCache,
			GitAuth:      app.GitAuth,
			BuildArgs:    app.BuildArgs,
			BuildSecrets: app.BuildSecrets,
		}, out)
		version = gitVersion(app.GitBranch, res.Commit)
	} else {
		imageTag = app.Image + ":" + app.Tag
		if app.PredeployCommand != "" && !j.skipPredeploy {
			imageTag, err = d.engine.ResolveDigest(ctx, imageTag, app.RegistryAuth)
			if err == nil && imageTag == "" {
				err = fmt.Errorf("registry did not return an immutable image")
			}
		} else {
			imageTag = resolveImageRef(ctx, d.engine, app, imageTag)
		}
		fmt.Fprintf(out, "→ deploy image %s\n", imageTag)
		version = imageVersion(app.Tag, imageTag)
	}
	if err == nil {
		d.recordVersion(deployID, version)
	}

	// Resolve local builds once. The same immutable image goes into predeploy,
	// the service and the encrypted release snapshot.
	if err == nil && j.rollbackID == 0 && j.applyImage == "" && app.SourceType == "dockerfile" && ((app.PredeployCommand != "" && !j.skipPredeploy) || (releaseCapable && secret.Enabled())) {
		if runner, ok := d.engine.(docker.PredeployRunner); ok {
			imageTag, err = runner.LocalImageID(ctx, imageTag)
		} else if app.PredeployCommand != "" && !j.skipPredeploy {
			err = ErrReleaseUnavailable
		}
	}

	// Builds do not hold this guard: a user may change placement while building.
	// Refuse that stale build, then hold the guard through label/service mutation
	// and convergence so a placement save cannot expand the eligible node set.
	if err == nil {
		key := oplock.AppPlacement(app.ID)
		if err = oplock.Acquire(ctx, key); err == nil {
			defer oplock.Release(key)
			if j.rollbackID == 0 {
				var mode string
				var nodes []string
				var e error
				if placement, ok := d.store.(interface {
					Placement(context.Context, int64) (string, []string, error)
				}); ok {
					mode, nodes, e = placement.Placement(ctx, app.ID)
				} else {
					var current App
					current, e = d.store.GetApplication(ctx, app.ID)
					mode, nodes = current.PlacementMode, current.PlacementNodes
				}
				if e != nil {
					err = e
				} else if mode != app.PlacementMode || !reflect.DeepEqual(nodes, app.PlacementNodes) {
					err = fmt.Errorf("placement changed during deployment; deploy the current settings again")
				}
			}
		}
	}
	if err == nil && releaseCapable && secret.Enabled() && immutableImage(imageTag) {
		snapshot = d.snapshotFor(app, imageTag)
		err = rs.SaveRelease(ctx, app.ID, deployID, snapshot, j.rollbackID)
		releaseSaved = err == nil
	}
	// New configuration-only clones have no node labels until explicit Deploy.
	// Failed/ambiguous mutations remain associated with this existing app ID;
	// cloning itself never creates labels requiring external compensation.
	// An apply changes nothing — not the placement labels, not through the
	// predeploy command or the volume chown below — before it knows the
	// history it was queued on still holds, the service still runs, settled,
	// the image it was queued for, and a local build is on every node the app
	// is to run on.
	if err == nil && j.applyImage != "" {
		err = d.checkApplyFresh(ctx, j, app)
		if err == nil {
			var p docker.ServiceProgress
			if p, err = d.engine.ServiceProgress(ctx, docker.ServiceName(app.ID), nil); err == nil {
				if e := checkApplyTarget(p, imageTag); e != nil {
					err = applySkip{e}
				}
			}
		}
		if err == nil {
			if e := d.validateApplyImage(ctx, app, imageTag, false); e != nil {
				err = applySkip{e}
			}
		}
	}
	if err == nil && j.rollbackID == 0 {
		err = d.ensurePlacement(ctx, app)
	}

	if err == nil && app.PredeployCommand != "" && !j.skipPredeploy {
		err = d.predeploy(ctx, app, deployID, imageTag, out)
	}

	// Both checks use the captured desired settings, not updated_at (status
	// polling legitimately changes that timestamp while a deployment runs).
	if err == nil && expected != nil {
		current, cerr := rs.RollbackApplication(ctx, app.ID, imageTag)
		if cerr != nil || !reflect.DeepEqual(current.Runtime, *expected) || !reflect.DeepEqual(guards(current), snapshot.Guards) {
			err = ErrReleaseDrift
		} else {
			app = applyRelease(current, snapshot)
			err = d.engine.(docker.ReleaseValidator).ValidateRelease(ctx, d.buildSpec(app, imageTag), app.PlacementNodes)
		}
	}
	complete := func(ctx context.Context) {
		if releaseSaved {
			var restore *RuntimeSettings
			if expected != nil {
				if e := rs.CheckRollback(ctx, app.ID, *expected, snapshot.Guards, imageTag); e != nil {
					fmt.Fprintln(out, ErrReleaseDrift.Error())
					d.finish(ctx, deployID, app.ID, StatusError, imageTag, ErrReleaseDrift.Error())
					return
				}
				restore = &snapshot.Runtime
			}
			if e := rs.ConfirmRelease(ctx, app.ID, deployID, expected, restore); e != nil {
				msg := "could not confirm release or restore settings; concurrent changes were preserved"
				fmt.Fprintln(out, msg)
				d.finish(ctx, deployID, app.ID, StatusError, imageTag, msg)
				return
			}
		}
		d.recordAppliedConfig(deployID, j, app)
		fmt.Fprintf(out, "✅ deployed %s\n", imageTag)
		d.finish(ctx, deployID, app.ID, StatusRunning, imageTag, "")
	}

	// Chown owned volumes to the app's runtime uid:gid before the task starts
	// (fresh named volumes are root:root; a non-root app can't write). Runs on
	// the app's placement node(s); a failure fails the deploy.
	if err == nil {
		err = d.chownOwnedVolumes(ctx, app, out)
	}

	var baseline []string
	if err == nil {
		err = func() error {
			key := oplock.AppRouting(app.ID)
			if err := oplock.Acquire(ctx, key); err != nil {
				return err
			}
			defer oplock.Release(key)
			if expected != nil {
				if err := rs.CheckRollback(ctx, app.ID, *expected, snapshot.Guards, imageTag); err != nil {
					return err
				}
			}
			if routes, ok := d.store.(interface {
				RefreshRouting(context.Context, *App) error
			}); ok {
				if err := routes.RefreshRouting(ctx, &app); err != nil {
					return fmt.Errorf("refresh app routing: %w", err)
				}
			}
			// Snapshot immediately before apply, after waiting for routing and
			// refreshing it. A reload during that wait can start another OLD-image
			// task, which must also be excluded from this release's convergence.
			// A failed snapshot is never an empty baseline (that would count old
			// tasks as new). Found=false is legitimate on the first deploy.
			prev, err := d.engine.ServiceProgress(ctx, docker.ServiceName(app.ID), nil)
			if err != nil {
				return fmt.Errorf("snapshot running tasks before deploy: %w", err)
			}
			if prev.Found {
				baseline = prev.TaskIDs
			}
			// The migration chose this app while it ran, but it may have been
			// stopped since (this deploy can wait long for a queue slot):
			// moving it now would start it again.
			// A global service is removed rather than scaled to zero, so a
			// missing service counts as stopped too (the migration never moves
			// an app that was not deployed).
			if j.system && (!prev.Found || (!prev.Global && prev.Desired == 0)) {
				return errStoppedMeanwhile
			}
			// The egress network and the sidecar's names on it come first:
			// an app on that network whose hosts the sidecar does not
			// answer for yet would reach them directly.
			if len(app.LinkEgress) > 0 {
				if d.egress == nil {
					return ErrEgressUnavailable
				}
				if err := d.egress.Prepare(ctx, app.ID, app.LinkEgress); err != nil {
					return fmt.Errorf("prepare the app's link egress: %w", err)
				}
			}
			if expected != nil {
				if err := d.engine.(docker.ReleaseValidator).ValidateRelease(ctx, d.buildSpec(app, imageTag), app.PlacementNodes); err != nil {
					return err
				}
			}
			// Again under the lock a Stop scales under: a Stop that ended
			// before this apply was admitted found nothing to cancel.
			if j.applyImage != "" {
				if err := checkApplyTarget(prev, imageTag); err != nil {
					return applySkip{err}
				}
				if err := d.validateApplyImage(ctx, app, imageTag, true); err != nil {
					return applySkip{err}
				}
			}
			// A deploy of this app still watched in the background (only a
			// system deploy can get here meanwhile) is superseded now, just
			// before its service is replaced — not earlier, so a deploy that
			// fails while preparing leaves that rollout's watch alone.
			if err := d.enterApply(deployID); err != nil {
				return err
			}
			d.supersedeSettle(app.ID, deployID)
			return d.engine.ServiceDeploy(ctx, d.buildSpec(app, imageTag))
		}()
	}

	if err != nil && d.stopRecorded(deployID) {
		// Stopped before it applied anything: StopApp records it.
		slog.Info("deploy ended by a stop of the app", "deploy", deployID, "app", app.ID, "err", err)
		return
	}
	if j.applyImage != "" && errors.As(err, new(applySkip)) {
		fmt.Fprintf(out, "⏹ %v\n", err)
		slog.Info("settings apply skipped", "deploy", deployID, "app", app.ID, "err", err)
		d.finishApplySkipped(deployID, app.ID, imageTag, err.Error())
		return
	}
	if errors.Is(err, errStoppedMeanwhile) {
		fmt.Fprintf(out, "⏹ %v\n", err)
		slog.Info("system deploy skipped, the app was stopped", "deploy", deployID, "app", app.ID)
		d.finishAs(deployID, app.ID, "error", "", imageTag, err.Error())
		return
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
	converged, crashed, rolledBack, stopped := false, false, false, false
	firstPoll := true
	for {
		st, serr := d.engine.ServiceProgress(cctx, docker.ServiceName(app.ID), baseline)
		// The app was stopped after this deploy applied its spec: the service
		// is down on purpose, nothing is left to wait for.
		if serr == nil && (!st.Found || (st.Desired == 0 && !st.Global)) && d.stoppedBy(deployID) != "" {
			stopped = true
			break
		}
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
		if serr == nil && st.Found && st.Desired > 0 && st.Running >= st.Desired && (st.UpdateState == "" || st.UpdateState == "completed") {
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
		complete(ctx)
		return
	}
	if stopped {
		msg := "stopped by " + d.stoppedBy(deployID) + " before the service converged"
		fmt.Fprintf(out, "⏹ %s\n", msg)
		d.finishAs(deployID, app.ID, "error", "", imageTag, msg)
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
	// an error.
	if ctx.Err() != nil && d.stopping() {
		fmt.Fprintf(out, "⚠ deploy interrupted (server shutting down)\n")
		if j.rollbackID != 0 {
			d.finish(ctx, deployID, app.ID, StatusError, imageTag, "rollback interrupted before confirmation")
		} else {
			d.finishInterrupted(deployID, app.ID, imageTag)
		}
		return
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	st, serr := d.engine.ServiceProgress(fctx, docker.ServiceName(app.ID), baseline)
	fcancel()
	// The job's own deadline may be what ended the wait: confirming a release
	// that converged at this last look needs a live context.
	confirmCtx := ctx
	if ctx.Err() != nil {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		confirmCtx = c
	}
	switch {
	case serr == nil && st.Found && strings.HasPrefix(st.UpdateState, "rollback"):
		fmt.Fprintf(out, "❌ rolling update failed — swarm rolled back to the previous version\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "update rolled back")
	case serr == nil && st.Found && st.Desired > 0 && st.Running >= st.Desired && (st.UpdateState == "" || st.UpdateState == "completed"):
		complete(confirmCtx)
	case serr == nil && st.Found && st.Failed > 0 && st.Running < st.Desired:
		fmt.Fprintf(out, "❌ service has failed tasks (crash-looping) — check the image, command and env\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "service crash-looping")
	case serr == nil && st.Found && (st.Desired > 0 || st.Global):
		// Not ready yet and no failure seen: a new task still pulling its image
		// (on a first deploy Swarm reports no update status at all), a rolling
		// update still in flight, or the job's own deadline ran out while
		// waiting. The deployment stays running and is watched in the
		// background until Swarm actually finishes — recording it done now
		// would claim a release that may still be pulling its image.
		fmt.Fprintf(out, "⚠ service still starting after %s — still waiting, up to %s more\n", timeout, settleTimeout)
		d.startSettle(deployID, app.ID, j.rollbackID != 0, imageTag, docker.ServiceName(app.ID), baseline, timeout, out, complete)
	default:
		fmt.Fprintf(out, "❌ service did not become healthy in time\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "service did not converge")
	}
}

func (d *Deployer) stopping() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// settleWatch is one deploy being watched in the background.
type settleWatch struct {
	deployID int64
	cancel   context.CancelCauseFunc
	done     chan struct{}
}

// errSuperseded ends a background watch because another deploy of the same
// app started: the service now runs that deploy's spec, so this one's outcome
// can no longer be told apart from it.
var errSuperseded = errors.New("superseded by a newer deploy of this app")

// supersedeSettle stops the background watch of appID, if any, before a new
// deploy of that app touches its service, and waits for it to record itself.
func (d *Deployer) supersedeSettle(appID, by int64) {
	d.settleMu.Lock()
	w := d.settling[appID]
	d.settleMu.Unlock()
	if w == nil || w.deployID == by {
		return
	}
	w.cancel(errSuperseded)
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		slog.Warn("background deploy watch did not stop in time", "app", appID, "deploy", w.deployID)
	}
}

// Settling reports whether deployID applied its spec and is being watched in
// the background while Swarm converges (startSettle): the deployment is still
// running, but nothing of it is left to build or apply.
func (d *Deployer) Settling(deployID int64) bool {
	d.settleMu.Lock()
	defer d.settleMu.Unlock()
	for _, w := range d.settling {
		if w.deployID == deployID {
			return true
		}
	}
	return false
}

// startSettle keeps watching a deploy that had not converged by its deadline,
// off the build worker, and records the real outcome: done once Swarm reports
// the update completed with every task running, error on a rollback, a crash
// loop or settleTimeout. The deployment stays running meanwhile, so the app
// takes no other tenant deploy until this one is decided. A stop of the app,
// a newer (system) deploy of it and a Krill shutdown end the watch early.
func (d *Deployer) startSettle(deployID, appID int64, rollback bool, imageTag, service string, baseline []string, waited time.Duration, out io.Writer, complete func(context.Context)) {
	parent := d.runCtx
	if parent == nil {
		parent = context.Background()
	}
	cctx, cancelCause := context.WithCancelCause(parent)
	w := &settleWatch{deployID: deployID, cancel: cancelCause, done: make(chan struct{})}
	d.settleMu.Lock()
	if d.settling == nil {
		d.settling = map[int64]*settleWatch{}
	}
	d.settling[appID] = w
	d.settleMu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(w.done)
		defer func() {
			d.settleMu.Lock()
			if d.settling[appID] == w {
				delete(d.settling, appID)
			}
			d.settleMu.Unlock()
		}()
		defer cancelCause(nil)
		ctx, cancel := context.WithTimeout(cctx, settleTimeout)
		defer cancel()
		started := time.Now()
		for {
			st, err := d.engine.ServiceProgress(ctx, service, baseline)
			switch {
			case err != nil:
				// Transient (a manager hiccup): keep polling until the deadline.
			case !st.Found || (st.Desired == 0 && !st.Global):
				// Stopped (scaled to zero, or a global service removed) or deleted
				// while starting. The app's status is the stop's to set. A global
				// service with no new task yet is waiting for an eligible node.
				fmt.Fprintf(out, "⚠ the service was stopped before it converged\n")
				d.finishAs(deployID, appID, "error", "", imageTag, "stopped before the service converged")
				return
			case st.Found && strings.HasPrefix(st.UpdateState, "rollback"):
				fmt.Fprintf(out, "❌ rolling update failed — swarm rolled back to the previous version\n")
				d.finish(ctx, deployID, appID, StatusError, imageTag, "update rolled back")
				return
			case st.Found && st.Desired > 0 && st.Running >= st.Desired && (st.UpdateState == "" || st.UpdateState == "completed"):
				fmt.Fprintf(out, "→ service converged after %s\n", (waited + time.Since(started)).Round(time.Second))
				complete(ctx)
				return
			case st.Found && st.Failed >= 3 && st.Running < st.Desired:
				fmt.Fprintf(out, "❌ service is crash-looping (tasks keep failing) — check the image, command and env\n")
				d.finish(ctx, deployID, appID, StatusError, imageTag, "service crash-looping")
				return
			}
			select {
			case <-ctx.Done():
				if errors.Is(context.Cause(cctx), errSuperseded) {
					fmt.Fprintf(out, "⚠ %s\n", errSuperseded)
					d.finishAs(deployID, appID, "error", "", imageTag, errSuperseded.Error())
					return
				}
				if d.stopping() {
					fmt.Fprintf(out, "⚠ deploy interrupted (server shutting down)\n")
					if rollback {
						d.finishAs(deployID, appID, "error", StatusError, imageTag, "rollback interrupted before confirmation")
					} else {
						d.finishInterrupted(deployID, appID, imageTag)
					}
					return
				}
				total := (waited + settleTimeout).Round(time.Second)
				fmt.Fprintf(out, "❌ service did not converge within %s — Swarm may still finish it; check the app's tasks\n", total)
				d.finish(ctx, deployID, appID, StatusError, imageTag, "timed out waiting for the service to converge")
				return
			case <-time.After(settlePollInterval):
			}
		}
	}()
}

// finishInterrupted records a deploy that Krill stopped waiting for because it
// is shutting down: the deployment failed (its outcome is unknown), while the
// app is left deploying, and StartStatusHeal sets it from the service after
// restart.
func (d *Deployer) finishInterrupted(deployID, appID int64, imageTag string) {
	d.finishAs(deployID, appID, "error", StatusDeploying, imageTag, "interrupted: krill stopped before the service converged")
}

// finish closes the log hub, saves the log to deployments, and updates statuses.
func (d *Deployer) finish(_ context.Context, deployID, appID int64, status, imageTag, errMsg string) {
	depStatus := "done"
	if status == StatusError {
		depStatus = "error"
	}
	d.finishAs(deployID, appID, depStatus, status, imageTag, errMsg)
}

// finishAs records a deployment's outcome (depStatus: done or error) and the
// app's status separately; an empty appStatus leaves the app's status alone.
func (d *Deployer) finishAs(deployID, appID int64, depStatus, appStatus, imageTag, errMsg string) {
	record, stopped := d.settleRecord(deployID)
	if !record {
		return // a stop cancelled it and records it
	}
	defer d.finished(deployID)
	if stopped {
		// The app was stopped while this deploy was open: the stop owns the
		// app's status, and the deploy's outcome is no alert.
		appStatus = ""
	}
	full := d.hub.Close(deployID)
	status := appStatus
	// Detached context: persist the final status even if the job ctx was canceled
	// (graceful shutdown), so deployments don't get stuck as running/deploying.
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// What a successful deploy applied is written with its done status, in
	// one update: a row never reads done without it, nor failed with it.
	if c, ok := d.takeAppliedConfig(deployID); ok && depStatus == "done" {
		if err := d.store.(configStore).FinishDeploymentApplied(wctx, deployID, depStatus, imageTag, errMsg, full, c); err != nil {
			slog.Error("finish deployment failed", "deploy", deployID, "err", err)
		}
	} else if err := d.store.FinishDeployment(wctx, deployID, depStatus, imageTag, errMsg, full); err != nil {
		slog.Error("finish deployment failed", "deploy", deployID, "err", err)
	}
	if appID != 0 && appStatus != "" {
		_ = d.store.SetStatus(wctx, appID, appStatus)
	}
	// Detached context: the notifier resolves + sends asynchronously and must
	// survive a canceled job ctx (graceful shutdown right after a failed deploy).
	if status == StatusError && appID != 0 && d.notifier != nil {
		d.notifier.DeployFailed(context.Background(), appID, errMsg)
	}
	if status == StatusRunning && appID != 0 && d.notifier != nil {
		d.notifier.DeploySucceeded(context.Background(), deployID)
	}
	if appID != 0 && d.egress != nil {
		d.egress.Changed(appID)
	}
}

// EgressNetworkPrefix names the Krill link's per-app egress networks
// (krill-egress-<app>, link.EgressNetwork).
const EgressNetworkPrefix = "krill-egress-"

// LinkEgressLabel is the service label holding the link selection an app's
// current spec was built with (link.egressLabel reads it).
const LinkEgressLabel = "krill.link.egress"

// EgressNetwork is an app's egress network.
func EgressNetwork(appID int64) string {
	return EgressNetworkPrefix + strconv.FormatInt(appID, 10)
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
	// The app's own egress network is managed here: attached while a link
	// selection is applied, left otherwise (the link was turned off, the
	// organization revoked, the selection emptied). The release before
	// egress keeps every krill-egress-* attachment instead
	// (KeepNetworkPrefix), which is what makes a rollback onto it fail
	// closed. Traefik still routes over net: AppLabels names it with
	// traefik.docker.network, and the gateway is not on the egress network.
	if len(app.LinkEgress) > 0 {
		spec.Networks = []string{net, EgressNetwork(app.ID)}
		// The selection this spec is built with, on the service itself: the
		// link keeps these names on the sidecar while the service carries
		// them, also when Krill restarts before the deploy is recorded.
		spec.Labels[LinkEgressLabel] = strings.Join(app.LinkEgress, ",")
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

// versionStore is the optional store capability that records a deployment's
// version (the database store has it; small test stores need not).
type versionStore interface {
	SetDeploymentVersion(ctx context.Context, deployID int64, version string) error
	DeploymentVersion(ctx context.Context, deployID int64) (string, error)
}

// recordVersion stores the version a deployment ships. Best-effort: the
// version only labels the deploy in messages, so a failed write is logged
// and the deploy goes on.
func (d *Deployer) recordVersion(deployID int64, version string) {
	vs, ok := d.store.(versionStore)
	if !ok || version == "" {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := vs.SetDeploymentVersion(wctx, deployID, version); err != nil {
		slog.Warn("record deployment version failed", "deploy", deployID, "err", err)
	}
}

// sourceVersion is the version of the deployment a rollback returns to; ""
// when it has none (made before versions were recorded) or cannot be read.
func (d *Deployer) sourceVersion(ctx context.Context, sourceID int64) string {
	vs, ok := d.store.(versionStore)
	if !ok {
		return ""
	}
	v, err := vs.DeploymentVersion(ctx, sourceID)
	if err != nil {
		slog.Warn("read rollback source version failed", "deploy", sourceID, "err", err)
		return ""
	}
	return v
}

// gitVersion names a Dockerfile build "branch@commit" ("main" when no branch
// is set, as the builder clones); just the branch when the commit is unknown.
func gitVersion(branch, commit string) string {
	if branch == "" {
		branch = "main"
	}
	if commit == "" {
		return branch
	}
	return branch + "@" + commit
}

// shortDigestLen is how many hex digits of an image digest a version shows.
const shortDigestLen = 12

// pinnedTag reports whether a tag looks like it names a single build: a full
// version (major.minor.patch, as in v1.8.3 or 16.4.1-alpine) or a commit hash
// of at least seven hex digits (sha-1a09ec0). Anything else may be moved to
// another image by a later push.
func pinnedTag(tag string) bool {
	if versionTagRe.MatchString(tag) {
		return true
	}
	h := commitTagRe.FindStringSubmatch(tag)
	return h != nil && strings.ContainsAny(h[1], "0123456789") && strings.ContainsAny(h[1], "abcdef")
}

// The whole tag has to be the version or the hash: "latest-for-1.2.3" moves.
var (
	versionTagRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)
	commitTagRe  = regexp.MustCompile(`^(?:[a-z]+-)?([0-9a-f]{7,64})$`)
)

// imageVersion names an image deploy by its tag. A tag that may move
// (latest, main, stable-2, v1, 16) also gets the short digest of the image it
// pointed at, so two deploys of the same moving tag can be told apart; a
// pinned one (pinnedTag) is shown alone.
func imageVersion(tag, ref string) string {
	if tag == "" {
		return ""
	}
	if pinnedTag(tag) {
		return tag
	}
	_, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return tag
	}
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > shortDigestLen {
		digest = digest[:shortDigestLen]
	}
	if digest == "" {
		return tag
	}
	return tag + " (" + digest + ")"
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

func (d *Deployer) ensurePlacement(ctx context.Context, app App) error {
	nodes, err := d.engine.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("load placement nodes: %w", err)
	}
	live := map[string]bool{}
	for _, n := range nodes {
		live[n.ID] = true
	}
	for _, id := range app.PlacementNodes {
		if !live[id] {
			return fmt.Errorf("placement node is missing: %s", id)
		}
	}
	key := fmt.Sprintf("krill.place.%d", app.ID)
	selected := map[string]bool{}
	if app.PlacementMode == "pin" || app.PlacementMode == "global" {
		for _, id := range app.PlacementNodes {
			selected[id] = true
		}
	}
	// Remove extras first: a failed add cannot leave more eligible nodes than
	// intended. Only this app's label key is touched.
	for _, node := range nodes {
		if !selected[node.ID] {
			if err := d.engine.NodeDeleteLabel(ctx, node.ID, key); err != nil {
				return fmt.Errorf("clear app placement label: %w", err)
			}
		}
	}
	for _, node := range nodes {
		if selected[node.ID] {
			if err := d.engine.NodeSetLabel(ctx, node.ID, key, "1"); err != nil {
				return fmt.Errorf("set app placement label: %w", err)
			}
		}
	}

	return nil
}
