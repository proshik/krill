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

// App — представление приложения для деплоя.
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

// Store — то, что нужно деплойеру от хранилища.
type Store interface {
	GetApplication(ctx context.Context, id int64) (App, error)
	GetDeploymentApp(ctx context.Context, deployID int64) (App, error)
	SetStatus(ctx context.Context, id int64, status string) error
	CreateDeployment(ctx context.Context, appID int64, trigger string) (int64, error)
	FinishDeployment(ctx context.Context, deployID int64, status, imageTag, errMsg, log string) error
}

const jobTimeout = 10 * time.Minute // сборка может быть дольше pull'а

// Deployer обрабатывает деплои через очередь и воркер.
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
				d.run(jobCtx, deployID)
				cancel()
			}
		}
	}()
}

// Enqueue создаёт deployment-запись и ставит её в очередь. Возвращает deployID (0 при ошибке/после Stop).
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
	d.stopOnce.Do(func() { close(d.done) })
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
	fmt.Fprintf(out, "✅ deployed %s\n", imageTag)
	d.finish(ctx, deployID, app.ID, StatusRunning, imageTag, "")
}

// finish закрывает лог-хаб, сохраняет лог в deployments и обновляет статусы.
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
