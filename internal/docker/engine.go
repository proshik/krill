package docker

import (
	"context"
	"io"
	"strconv"
)

// PortSpec — публикуемый порт сервиса.
type PortSpec struct {
	Target    uint32
	Published uint32
	Mode      string // "host" | "ingress"
	UDP       bool
}

// MountSpec — bind-монтирование.
type MountSpec struct {
	Source   string
	Target   string
	ReadOnly bool
}

// ServiceSpec — наше нейтральное описание Swarm-сервиса.
type ServiceSpec struct {
	Name        string
	Image       string // image:tag
	Command     []string
	Args        []string
	Env         map[string]string
	Labels      map[string]string // service-level (читает Traefik swarm-провайдер)
	Replicas    uint64
	Network     string
	Ports       []PortSpec
	Mounts      []MountSpec
	Constraints []string // напр. node.role==manager
}

// ServiceState — текущее состояние сервиса в Swarm.
type ServiceState struct {
	Found   bool
	Running int
	Desired int
}

// Engine — узкий интерфейс к Docker/Swarm (мокабельный).
type Engine interface {
	NetworkEnsure(ctx context.Context, name string) error
	ServiceDeploy(ctx context.Context, spec ServiceSpec) error // create-or-rolling-update by name
	ServiceRemove(ctx context.Context, name string) error
	ServiceState(ctx context.Context, name string) (ServiceState, error)
	ServiceLogs(ctx context.Context, name string, follow bool) (io.ReadCloser, error)
}

// ServiceName строит имя Swarm-сервиса для приложения по его id.
func ServiceName(appID int64) string {
	return "krill-" + strconv.FormatInt(appID, 10)
}
