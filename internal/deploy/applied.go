package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
)

// The settings sections a deploy applies, each one an editor tab or card the
// user can find: source (the settings tab's source and build cards), env (the
// variables tab, database and bucket links included), metrics (the metrics
// token the metrics tab sets), runtime (resources, command, healthcheck),
// predeploy (the pre-deploy command, apart because some deploys skip it),
// volumes, ports and placement (their settings cards). Domains are not here:
// they apply at once.
const (
	SectionSource    = "source"
	SectionEnv       = "env"
	SectionMetrics   = "metrics"
	SectionRuntime   = "runtime"
	SectionPredeploy = "predeploy"
	SectionVolumes   = "volumes"
	SectionPorts     = "ports"
	SectionPlacement = "placement"
)

// Sections lists every section in display order.
var Sections = []string{SectionSource, SectionEnv, SectionMetrics, SectionRuntime, SectionPredeploy, SectionVolumes, SectionPorts, SectionPlacement}

// fingerprintVersion changes when what a section covers changes; a stored
// fingerprint of another version is unknown, not changed.
const fingerprintVersion = 1

// fingerprintPurpose separates these digests from any other secret.Fingerprint use.
const fingerprintPurpose = "deploy config v1"

// AppliedConfig is the configuration fingerprint a successful deployment
// stores: one digest per section, made under the key named by Key. No value
// is ever kept, so it is safe to store and to compare.
type AppliedConfig struct {
	Version  int               `json:"v"`
	Key      string            `json:"key"`
	Sections map[string]string `json:"sections"`
}

// comparable reports whether c was made the way fingerprints are made now.
func (c AppliedConfig) comparable(now AppliedConfig) bool {
	return c.Version == now.Version && c.Key == now.Key && c.Sections != nil
}

// configSections is what each section covers, read from the App the deploy
// builds its service spec from (after withResourceDefaults). Both the stored
// and the current fingerprint go through it, so they cannot drift apart.
func configSections(a App) map[string]any {
	env := maps.Clone(a.Env)
	if env == nil {
		env = map[string]string{}
	}
	var metrics struct{ Var, Value string }
	if a.MetricsTokenEnv != "" {
		metrics.Var, metrics.Value = a.MetricsTokenEnv, env[a.MetricsTokenEnv]
		delete(env, a.MetricsTokenEnv)
	}

	var source any
	if a.SourceType == "dockerfile" {
		gitURL := a.GitURL
		if u, err := url.Parse(gitURL); err == nil && u.User != nil {
			u.User = nil
			gitURL = u.String()
		}
		// The builder's defaults: an empty branch clones main, an empty path
		// builds ./Dockerfile.
		branch, dockerfile := a.GitBranch, a.DockerfilePath
		if branch == "" {
			branch = "main"
		}
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}
		dockerfile = path.Clean(dockerfile) // ./Dockerfile is the same file
		source = struct {
			Type, GitURL, Branch, Dockerfile string
			BuildArgs, BuildSecrets          map[string]string
		}{a.SourceType, gitURL, branch, dockerfile, nonNil(a.BuildArgs), nonNil(a.BuildSecrets)}
	} else {
		source = struct{ Type, Image, Tag string }{a.SourceType, a.Image, a.Tag}
	}

	replicas := a.Replicas
	if replicas == 0 {
		replicas = 1 // as buildSpec deploys it
	}
	if a.PlacementMode == "global" && len(a.PlacementNodes) > 0 {
		replicas = 0 // a global service runs one task per node; buildSpec ignores the count
	}
	runtime := struct {
		Args               []string
		Port               int32
		Replicas           uint64
		MemoryLimitBytes   int64
		NanoCPUs           int64
		RestartCondition   string
		RestartMaxAttempts uint64
		Healthcheck        *docker.HealthcheckSpec
	}{slices.Clip(a.Args), a.Port, replicas, a.MemoryLimitBytes, a.NanoCPUs, a.RestartCondition, a.RestartMaxAttempts, a.Healthcheck}
	if len(runtime.Args) == 0 {
		runtime.Args = nil
	}

	mounts := slices.Clone(a.Mounts)
	slices.SortFunc(mounts, func(x, y docker.MountSpec) int {
		if x.Target != y.Target {
			return strings.Compare(x.Target, y.Target)
		}
		return strings.Compare(x.Source, y.Source)
	})
	ports := slices.Clone(a.Ports)
	slices.SortFunc(ports, func(x, y docker.PortSpec) int {
		if x.Published != y.Published {
			return int(x.Published) - int(y.Published)
		}
		if x.UDP != y.UDP {
			if x.UDP {
				return 1
			}
			return -1
		}
		return int(x.Target) - int(y.Target)
	})
	nodes := slices.Clone(a.PlacementNodes)
	slices.Sort(nodes)

	return map[string]any{
		SectionSource:    source,
		SectionEnv:       env,
		SectionMetrics:   metrics,
		SectionRuntime:   runtime,
		SectionPredeploy: a.PredeployCommand,
		SectionVolumes:   mounts,
		SectionPorts:     ports,
		SectionPlacement: struct {
			Mode  string
			Nodes []string
		}{a.PlacementMode, nodes},
	}
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// configFingerprint is the fingerprint of the configuration a deploy of a
// would apply, the instance-wide resource defaults included. The digests are
// bound to the app: equal settings of two apps give different digests, so
// one app's history says nothing about another's secrets.
func (d *Deployer) configFingerprint(a App) AppliedConfig {
	a = d.withResourceDefaults(a)
	out := AppliedConfig{Version: fingerprintVersion, Key: secret.FingerprintKeyID(), Sections: map[string]string{}}
	purpose := fingerprintPurpose + "/app:" + strconv.FormatInt(a.ID, 10) + "/"
	for name, v := range configSections(a) {
		b, err := json.Marshal(v)
		if err != nil {
			continue // unknown, never a false "changed"
		}
		out.Sections[name] = secret.Fingerprint(purpose+name, b)
	}
	return out
}

// configStore is the optional store capability behind applied-configuration
// tracking (the database store has it; small test stores need not).
type configStore interface {
	// FinishDeploymentApplied is FinishDeployment for a deploy that ended
	// done, recording what it applied in the same write.
	FinishDeploymentApplied(ctx context.Context, deployID int64, status, imageTag, errMsg, log string, c AppliedConfig) error
	DeploymentAppliedConfig(ctx context.Context, deployID int64) (AppliedConfig, bool, error)
	LatestApplied(ctx context.Context, appIDs []int64) (map[int64]LatestApplied, error)
}

// LatestApplied is an app's newest successful deployment.
type LatestApplied struct {
	DeploymentID int64
	ImageTag     string
	Config       *AppliedConfig // nil when that deployment recorded none
	InFlight     bool           // a deploy of the app is queued or running
}

// recordAppliedConfig works out what a successful deploy applied; finishAs
// writes it with the done status, in one update. What a deploy did not apply
// itself comes from the deployment it took it from: a rollback's app is
// loaded without its build settings (source) and skips the predeploy command,
// as the network migration's system deploy does, so those sections are the
// rollback source's, or the previous deploy's, or unknown.
func (d *Deployer) recordAppliedConfig(deployID int64, j job, app App) {
	cs, ok := d.store.(configStore)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := d.configFingerprint(app)
	var inherit []string
	if j.rollbackID != 0 {
		inherit = append(inherit, SectionSource)
	}
	if j.skipPredeploy {
		inherit = append(inherit, SectionPredeploy)
	}
	if len(inherit) > 0 {
		var from *AppliedConfig
		if j.rollbackID != 0 {
			if src, found, err := cs.DeploymentAppliedConfig(ctx, j.rollbackID); err == nil && found {
				from = &src
			}
		} else if latest, err := cs.LatestApplied(ctx, []int64{app.ID}); err == nil {
			// This deploy is not done yet: the latest done one is the previous.
			from = latest[app.ID].Config
		}
		for _, s := range inherit {
			delete(c.Sections, s)
			if from != nil && from.comparable(c) {
				if v, ok := from.Sections[s]; ok {
					c.Sections[s] = v
				}
			}
		}
	}
	d.appliedMu.Lock()
	if d.applied == nil {
		d.applied = map[int64]AppliedConfig{}
	}
	d.applied[deployID] = c
	d.appliedMu.Unlock()
}

// takeAppliedConfig hands finishAs what recordAppliedConfig worked out.
func (d *Deployer) takeAppliedConfig(deployID int64) (AppliedConfig, bool) {
	d.appliedMu.Lock()
	defer d.appliedMu.Unlock()
	c, ok := d.applied[deployID]
	delete(d.applied, deployID)
	return c, ok
}

// Pending is what an app's saved settings would change if deployed now.
type Pending struct {
	// Changed are the sections whose saved settings differ from what the
	// app's latest successful deployment applied, in Sections order.
	Changed []string
	// CanApply says "apply settings" can deploy them: the source is known and
	// unchanged and the running image is pinned (content-addressed).
	CanApply bool
	// image is the image apply settings would deploy.
	image string
}

// Any reports whether some section changed.
func (p Pending) Any() bool { return len(p.Changed) > 0 }

// Has reports whether section changed.
func (p Pending) Has(section string) bool { return slices.Contains(p.Changed, section) }

// pendingWorkers and pendingLimit bound what one page spends on pending
// changes: each app costs a full settings load (a few queries and decrypts).
const (
	pendingWorkers = 4
	pendingLimit   = 60
)

// PendingChanges compares each app's current settings with what its latest
// successful deployment applied. Apps that have none, whose fingerprint is
// unknown (made before it was recorded, or under another key), with a deploy
// in flight or whose settings cannot be loaded get no entry, nor do apps past
// the first pendingLimit. The caller decides which apps qualify (running).
func (d *Deployer) PendingChanges(ctx context.Context, appIDs []int64) map[int64]Pending {
	out := map[int64]Pending{}
	cs, ok := d.store.(configStore)
	if !ok || len(appIDs) == 0 {
		return out
	}
	if len(appIDs) > pendingLimit {
		appIDs = appIDs[:pendingLimit]
	}
	latest, err := cs.LatestApplied(ctx, appIDs)
	if err != nil {
		slog.Warn("pending changes: latest deployments", "err", err)
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, pendingWorkers)
	for _, id := range appIDs {
		l, ok := latest[id]
		if !ok || l.InFlight || l.Config == nil {
			continue
		}
		wg.Add(1)
		go func(id int64, l LatestApplied) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			p, ok := d.pendingFor(ctx, id, l)
			if ok {
				mu.Lock()
				out[id] = p
				mu.Unlock()
			}
		}(id, l)
	}
	wg.Wait()
	return out
}

func (d *Deployer) pendingFor(ctx context.Context, appID int64, l LatestApplied) (Pending, bool) {
	a, err := d.store.GetApplication(ctx, appID)
	if err != nil {
		slog.Warn("pending changes: load application settings", "app", appID, "err", err)
		return Pending{}, false
	}
	now := d.configFingerprint(a)
	if !l.Config.comparable(now) {
		return Pending{}, false
	}
	var p Pending
	for _, s := range Sections {
		stored, known := l.Config.Sections[s]
		if known && stored != now.Sections[s] {
			p.Changed = append(p.Changed, s)
		}
	}
	src, known := l.Config.Sections[SectionSource]
	p.CanApply = p.Any() && known && src == now.Sections[SectionSource] && docker.IsImmutableImage(l.ImageTag)
	p.image = l.ImageTag
	return p, true
}

var (
	// ErrNothingToApply: the app runs its current settings already, or what
	// it runs is not known (deployed before Krill recorded it).
	ErrNothingToApply = errors.New("no saved settings to apply: the running deployment has them")
	// ErrApplyNeedsDeploy: the image or build settings changed, or the running
	// image is not pinned by digest, so only a deploy can apply them.
	ErrApplyNeedsDeploy = errors.New("the image or build settings changed or the running image is not pinned: deploy instead")
	// ErrApplyTargetMoved: the service no longer runs, settled, the image the
	// apply was queued for (a newer deploy, a rollout Swarm finished after
	// Krill stopped watching, a rollback in progress).
	ErrApplyTargetMoved = errors.New("the service no longer runs the image this apply was queued for: deploy instead")
	// ErrApplyStopped: the app is stopped, or a Stop of it is under way.
	ErrApplyStopped = errors.New("the application is stopped: start it to deploy the saved settings")
)

// applySkip is an apply refused before it changed anything: it is recorded
// without an alert, and the app's status is read from its service again
// (finishApplySkipped) rather than set to error.
type applySkip struct{ error }

func (e applySkip) Unwrap() error { return e.error }

// checkApplyTarget reports whether an apply may replace the service p: it
// must run (a stopped app is the stop's: errStoppedMeanwhile), its last update
// must be settled and its spec must name exactly image. Deployment history can
// differ from Swarm, so only the live service says which image runs.
func checkApplyTarget(p docker.ServiceProgress, image string) error {
	if !p.Found || (!p.Global && p.Desired == 0) {
		return errStoppedMeanwhile
	}
	// No task running (it exited for good, or is crash-looping): the app
	// does not run, and an apply would start it.
	if p.Running == 0 || !docker.UpdateSettled(p.UpdateState) || p.Image != image {
		return ErrApplyTargetMoved
	}
	return nil
}

// ConfirmApply checks a Pending's CanApply against the live service, as the
// apply itself will: deployment history can name another image than the one
// that runs. One service read; for a single app's page.
func (d *Deployer) ConfirmApply(ctx context.Context, appID int64, p Pending) Pending {
	if !p.CanApply {
		return p
	}
	live, err := d.engine.ServiceProgress(ctx, docker.ServiceName(appID), nil)
	if err != nil || checkApplyTarget(live, p.image) != nil {
		p.CanApply = false
	}
	return p
}

// checkApplyFresh refuses an apply queued on history that has changed since:
// another deploy finished (a second apply of the same settings would run the
// predeploy command and roll the service again), or the saved settings went
// back to what runs.
func (d *Deployer) checkApplyFresh(ctx context.Context, j job, app App) error {
	cs, ok := d.store.(configStore)
	if !ok {
		return nil
	}
	latest, err := cs.LatestApplied(ctx, []int64{app.ID})
	if err != nil {
		return err
	}
	l := latest[app.ID]
	if l.DeploymentID != j.applyFrom {
		return applySkip{ErrApplyTargetMoved}
	}
	if l.Config != nil {
		now := d.configFingerprint(app)
		if l.Config.comparable(now) {
			for _, s := range Sections {
				if v, known := l.Config.Sections[s]; known && v != now.Sections[s] {
					return nil
				}
			}
			return applySkip{ErrNothingToApply}
		}
	}
	return nil
}

// finishApplySkipped records an apply refused before it changed anything:
// no alert, and the app's status from its service (the queue set it to
// deploying), idle when the service is at zero or gone.
func (d *Deployer) finishApplySkipped(deployID, appID int64, imageTag, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	st, err := d.engine.ServiceState(ctx, docker.ServiceName(appID))
	cancel()
	status := ""
	if err == nil {
		status = DeriveStatus(st, StatusIdle)
	}
	d.finishAs(deployID, appID, "error", status, imageTag, msg)
}

// EnqueueApplySettings deploys the app's current settings on the image its
// latest successful deployment runs, pinned by digest: no pull of a tag, no
// git clone, no build. It is refused like any deploy (ErrDeployInFlight, ...)
// and with ErrNothingToApply or ErrApplyNeedsDeploy when it does not fit. The
// worker checks the source again before it deploys.
func (d *Deployer) EnqueueApplySettings(ctx context.Context, appID int64, trigger string) (int64, error) {
	cs, ok := d.store.(configStore)
	if !ok {
		return 0, ErrNothingToApply
	}
	if n, err := d.store.CountRunningDeployments(ctx, appID); err != nil {
		return 0, fmt.Errorf("check deployments in progress: %w", err)
	} else if n > 0 {
		return 0, ErrDeployInFlight
	}
	latest, err := cs.LatestApplied(ctx, []int64{appID})
	if err != nil {
		return 0, err
	}
	l, ok := latest[appID]
	if !ok || l.Config == nil {
		return 0, ErrNothingToApply
	}
	p, ok := d.pendingFor(ctx, appID, l)
	if !ok || !p.Any() {
		return 0, ErrNothingToApply
	}
	if !p.CanApply {
		return 0, ErrApplyNeedsDeploy
	}
	live, err := d.engine.ServiceProgress(ctx, docker.ServiceName(appID), nil)
	if err != nil {
		return 0, fmt.Errorf("read the app's service: %w", err)
	}
	switch err := checkApplyTarget(live, l.ImageTag); {
	case errors.Is(err, errStoppedMeanwhile):
		return 0, ErrApplyStopped
	case err != nil:
		return 0, err
	}
	return d.enqueue(appID, trigger, job{applyImage: l.ImageTag, applyFrom: l.DeploymentID, applySource: l.Config.Sections[SectionSource]})
}

// validateApplyImage checks, for a locally built image (a sha256: image ID,
// which Swarm cannot pull), that it is on every node the app may run on. A
// registry digest needs no check: Swarm pulls it by digest where it lacks it.
// Volumes are left out: a new volume is what an apply may well add. Before
// the placement labels are reconciled (checkLabels false) only the nodes the
// app is to run on are checked, not whether their labels already agree.
func (d *Deployer) validateApplyImage(ctx context.Context, app App, image string, checkLabels bool) error {
	if !strings.HasPrefix(image, "sha256:") {
		return nil
	}
	v, ok := d.engine.(docker.ReleaseValidator)
	if !ok {
		return ErrReleaseUnavailable
	}
	spec := d.buildSpec(app, image)
	spec.Mounts = nil
	if !checkLabels {
		spec.Constraints = nil
	}
	var selected []string
	if app.PlacementMode == "pin" || app.PlacementMode == "global" {
		selected = app.PlacementNodes
	}
	if err := v.ValidateRelease(ctx, spec, selected); err != nil {
		return fmt.Errorf("the built image is not on every node the app may run on, deploy instead: %w", err)
	}
	return nil
}

// AppsDeploying reports which of the apps (those with a successful
// deployment) have a deploy queued or running, in one query: a page that
// does not look pending changes up can still hide their markers then.
func (d *Deployer) AppsDeploying(ctx context.Context, appIDs []int64) map[int64]bool {
	out := map[int64]bool{}
	cs, ok := d.store.(configStore)
	if !ok || len(appIDs) == 0 {
		return out
	}
	latest, err := cs.LatestApplied(ctx, appIDs)
	if err != nil {
		slog.Warn("pending changes: deploys in flight", "err", err)
		return out
	}
	for id, l := range latest {
		if l.InFlight {
			out[id] = true
		}
	}
	return out
}
