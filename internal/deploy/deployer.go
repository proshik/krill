package deploy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
	convergeTimeout      = 90 * time.Second
	convergePollInterval = 1 * time.Second
)

// Deployer handles deployments through a queue and a worker.
type Deployer struct {
	engine   docker.Engine
	builder  builder.Builder
	store    Store
	hub      *DeployLogHub
	network  string
	queue    chan int64 // deployID
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
		queue:   make(chan int64, 64),
		done:    make(chan struct{}),
	}
}

func (d *Deployer) Start(ctx context.Context) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.done:
				return
			case deployID := <-d.queue:
				jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
				d.mu.Lock()
				d.cancelJob = cancel
				d.mu.Unlock()
				d.run(jobCtx, deployID)
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
	case d.queue <- deployID:
	case <-d.done:
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

func (d *Deployer) run(ctx context.Context, deployID int64) {
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
		}, out)
	} else {
		imageTag = app.Image + ":" + app.Tag
		fmt.Fprintf(out, "→ deploy image %s\n", imageTag)
	}

	if err == nil {
		err = d.engine.ServiceDeploy(ctx, d.buildSpec(app, imageTag))
	}

	if err != nil {
		fmt.Fprintf(out, "❌ %v\n", err)
		slog.Error("deploy failed", "deploy", deployID, "err", err)
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, err.Error())
		return
	}
	fmt.Fprintf(out, "→ waiting for service to converge...\n")
	cctx, ccancel := context.WithTimeout(ctx, convergeTimeout)
	defer ccancel()
	converged := false
	for {
		st, serr := d.engine.ServiceState(cctx, docker.ServiceName(app.ID))
		if serr == nil && st.Found && st.Desired > 0 && st.Running >= st.Desired {
			converged = true
			break
		}
		select {
		case <-cctx.Done():
		case <-time.After(convergePollInterval):
			continue
		}
		break
	}
	if !converged {
		fmt.Fprintf(out, "❌ service did not become healthy in time\n")
		d.finish(ctx, deployID, app.ID, StatusError, imageTag, "service did not converge")
		return
	}
	fmt.Fprintf(out, "✅ deployed %s\n", imageTag)
	d.finish(ctx, deployID, app.ID, StatusRunning, imageTag, "")
}

// finish closes the log hub, saves the log to deployments, and updates statuses.
func (d *Deployer) finish(ctx context.Context, deployID, appID int64, status, imageTag, errMsg string) {
	full := d.hub.Close(deployID)
	depStatus := "done"
	appStatus := StatusRunning
	if status == StatusError {
		depStatus = "error"
		appStatus = StatusError
	}
	if err := d.store.FinishDeployment(ctx, deployID, depStatus, imageTag, errMsg, full); err != nil {
		slog.Error("finish deployment failed", "deploy", deployID, "err", err)
	}
	if appID != 0 {
		_ = d.store.SetStatus(ctx, appID, appStatus)
	}
}

func (d *Deployer) buildSpec(app App, imageTag string) docker.ServiceSpec {
	name := docker.ServiceName(app.ID)
	return docker.ServiceSpec{
		Name:     name,
		Image:    imageTag,
		Env:      app.Env,
		Labels:   traefik.AppLabels(name, app.Domain, app.Port, d.network),
		Replicas: 1,
		Network:  d.network,
	}
}

var _ io.Writer = (*hubWriter)(nil)
