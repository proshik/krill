package deploy

import (
	"context"
	"log/slog"
	"sync"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// App — данные приложения, нужные деплою.
type App struct {
	ID     int64
	Name   string
	Image  string
	Tag    string
	Domain string
	Port   int32
	Env    map[string]string
}

// AppStore — то, что деплою нужно от хранилища.
type AppStore interface {
	GetApplication(ctx context.Context, id int64) (App, error)
	SetStatus(ctx context.Context, id int64, status string) error
}

// Deployer — очередь деплоев + воркеры.
type Deployer struct {
	engine     docker.Engine
	store      AppStore
	network    string
	baseDomain string
	queue      chan int64
	wg         sync.WaitGroup
}

// New создаёт Deployer.
func New(engine docker.Engine, store AppStore, network, baseDomain string) *Deployer {
	return &Deployer{
		engine:     engine,
		store:      store,
		network:    network,
		baseDomain: baseDomain,
		queue:      make(chan int64, 64),
	}
}

// Start запускает n воркеров.
func (d *Deployer) Start(n int) {
	for i := 0; i < n; i++ {
		d.wg.Add(1)
		go d.worker()
	}
}

// Enqueue ставит приложение в очередь на деплой.
func (d *Deployer) Enqueue(appID int64) {
	d.queue <- appID
}

// Stop закрывает очередь и ждёт завершения воркеров.
func (d *Deployer) Stop() {
	close(d.queue)
	d.wg.Wait()
}

func (d *Deployer) worker() {
	defer d.wg.Done()
	for appID := range d.queue {
		d.deploy(appID)
	}
}

func (d *Deployer) deploy(appID int64) {
	ctx := context.Background()
	app, err := d.store.GetApplication(ctx, appID)
	if err != nil {
		slog.Error("deploy: get application", "id", appID, "err", err)
		return
	}
	_ = d.store.SetStatus(ctx, appID, StatusDeploying)

	spec := d.buildSpec(app)
	if err := d.engine.ServiceDeploy(ctx, spec); err != nil {
		slog.Error("deploy: service deploy", "id", appID, "err", err)
		_ = d.store.SetStatus(ctx, appID, StatusError)
		return
	}
	_ = d.store.SetStatus(ctx, appID, StatusRunning)
}

func (d *Deployer) buildSpec(app App) docker.ServiceSpec {
	name := docker.ServiceName(app.Name)
	image := app.Image + ":" + app.Tag
	labels := traefik.AppLabels(name, app.Domain, app.Port, d.network)
	return docker.ServiceSpec{
		Name:     name,
		Image:    image,
		Env:      app.Env,
		Labels:   labels,
		Replicas: 1,
		Network:  d.network,
		Ports:    nil,
	}
}
