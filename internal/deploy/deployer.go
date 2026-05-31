package deploy

import (
	"context"
	"log/slog"
	"sync"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// App — представление приложения для деплоя.
type App struct {
	ID     int64
	Name   string
	Image  string
	Tag    string
	Domain string
	Port   int32
	Env    map[string]string
}

// AppStore — то, что нужно деплойеру от хранилища.
type AppStore interface {
	GetApplication(ctx context.Context, id int64) (App, error)
	SetStatus(ctx context.Context, id int64, status string) error
}

// Deployer обрабатывает задачи деплоя через очередь и воркер.
type Deployer struct {
	engine  docker.Engine
	store   AppStore
	network string
	queue   chan int64
	wg      sync.WaitGroup
}

func New(engine docker.Engine, store AppStore, network string) *Deployer {
	return &Deployer{
		engine:  engine,
		store:   store,
		network: network,
		queue:   make(chan int64, 64),
	}
}

// Start запускает воркер.
func (d *Deployer) Start(ctx context.Context) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for id := range d.queue {
			if err := d.deploy(ctx, id); err != nil {
				slog.Error("deploy failed", "app", id, "err", err)
				_ = d.store.SetStatus(ctx, id, StatusError)
				continue
			}
			_ = d.store.SetStatus(ctx, id, StatusRunning)
		}
	}()
}

// Enqueue помечает приложение как deploying и ставит в очередь.
func (d *Deployer) Enqueue(appID int64) {
	_ = d.store.SetStatus(context.Background(), appID, StatusDeploying)
	d.queue <- appID
}

// Stop закрывает очередь и дожидается воркера.
func (d *Deployer) Stop() {
	close(d.queue)
	d.wg.Wait()
}

func (d *Deployer) deploy(ctx context.Context, appID int64) error {
	app, err := d.store.GetApplication(ctx, appID)
	if err != nil {
		return err
	}
	return d.engine.ServiceDeploy(ctx, d.buildSpec(app))
}

func (d *Deployer) buildSpec(app App) docker.ServiceSpec {
	name := docker.ServiceName(app.Name)
	return docker.ServiceSpec{
		Name:     name,
		Image:    app.Image + ":" + app.Tag,
		Env:      app.Env,
		Labels:   traefik.AppLabels(name, app.Domain, app.Port, d.network),
		Replicas: 1,
		Network:  d.network,
	}
}
