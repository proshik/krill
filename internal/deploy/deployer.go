package deploy

import (
	"context"
	"log/slog"
	"sync"
	"time"

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

// jobTimeout ограничивает время одного деплоя (защита от зависшего pull образа).
const jobTimeout = 5 * time.Minute

// Deployer обрабатывает задачи деплоя через очередь и воркер.
type Deployer struct {
	engine   docker.Engine
	store    AppStore
	network  string
	queue    chan int64
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func New(engine docker.Engine, store AppStore, network string) *Deployer {
	return &Deployer{
		engine:  engine,
		store:   store,
		network: network,
		queue:   make(chan int64, 64),
		done:    make(chan struct{}),
	}
}

// Start запускает воркер. Воркер завершается при вызове Stop.
func (d *Deployer) Start(ctx context.Context) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.done:
				return
			case id := <-d.queue:
				jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
				err := d.deploy(jobCtx, id)
				cancel()
				if err != nil {
					slog.Error("deploy failed", "app", id, "err", err)
					_ = d.store.SetStatus(ctx, id, StatusError)
					continue
				}
				_ = d.store.SetStatus(ctx, id, StatusRunning)
			}
		}
	}()
}

// Enqueue помечает приложение как deploying и ставит в очередь.
// Безопасно вызывать после Stop — задача игнорируется (без паники).
func (d *Deployer) Enqueue(appID int64) {
	select {
	case <-d.done:
		return // идёт остановка — новые задачи не принимаем
	default:
	}
	_ = d.store.SetStatus(context.Background(), appID, StatusDeploying)
	select {
	case d.queue <- appID:
	case <-d.done:
	}
}

// Stop сигнализирует воркеру об остановке и дожидается его. Идемпотентен.
func (d *Deployer) Stop() {
	d.stopOnce.Do(func() { close(d.done) })
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
	name := docker.ServiceName(app.ID)
	return docker.ServiceSpec{
		Name:     name,
		Image:    app.Image + ":" + app.Tag,
		Env:      app.Env,
		Labels:   traefik.AppLabels(name, app.Domain, app.Port, d.network),
		Replicas: 1,
		Network:  d.network,
	}
}
