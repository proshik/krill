package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

type dockerEngine struct {
	cli *client.Client
}

// NewEngine creates a real Engine. If host is empty → DOCKER_HOST/the default socket is used.
func NewEngine(host string) (Engine, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	return &dockerEngine{cli: cli}, nil
}

func (e *dockerEngine) NetworkEnsure(ctx context.Context, name string) error {
	list, err := e.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return err
	}
	for _, n := range list {
		if n.Name == name { // the name filter is a substring match, so compare exactly
			return nil
		}
	}
	_, err = e.cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "overlay", Attachable: true})
	return err
}

// findService finds a service by its exact name.
func (e *dockerEngine) findService(ctx context.Context, name string) (swarm.Service, bool, error) {
	svcs, err := e.cli.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return swarm.Service{}, false, err
	}
	for _, s := range svcs {
		if s.Spec.Name == name {
			return s, true, nil
		}
	}
	return swarm.Service{}, false, nil
}

func (e *dockerEngine) ServiceDeploy(ctx context.Context, spec ServiceSpec) error {
	sw := buildSwarmSpec(spec)
	cur, found, err := e.findService(ctx, spec.Name)
	if err != nil {
		return err
	}
	if !found {
		_, err = e.cli.ServiceCreate(ctx, sw, swarm.ServiceCreateOptions{})
		return err
	}
	// ForceUpdate+1 — so that even an unchanged tag triggers a rolling-update (redeploy).
	sw.TaskTemplate.ForceUpdate = cur.Spec.TaskTemplate.ForceUpdate + 1
	_, err = e.cli.ServiceUpdate(ctx, cur.ID, cur.Version, sw, swarm.ServiceUpdateOptions{})
	return err
}

func (e *dockerEngine) ServiceRemove(ctx context.Context, name string) error {
	return e.cli.ServiceRemove(ctx, name)
}

func (e *dockerEngine) ServiceState(ctx context.Context, name string) (ServiceState, error) {
	svc, found, err := e.findService(ctx, name)
	if err != nil {
		return ServiceState{}, err
	}
	if !found {
		return ServiceState{Found: false}, nil
	}
	desired := 0
	if r := svc.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
		desired = int(*r.Replicas)
	}
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", name),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return ServiceState{}, err
	}
	running := 0
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateRunning {
			running++
		}
	}
	return ServiceState{Found: true, Running: running, Desired: desired}, nil
}

func (e *dockerEngine) ServiceLogs(ctx context.Context, name string, follow bool) (io.ReadCloser, error) {
	rc, err := e.cli.ServiceLogs(ctx, name, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       "200",
	})
	if err != nil {
		return nil, err
	}
	return NewLogReader(rc), nil
}

func (e *dockerEngine) ImagePull(ctx context.Context, ref string, out io.Writer) error {
	rc, err := e.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(out, rc)
	return err
}

func (e *dockerEngine) VolumeRemove(ctx context.Context, name string) error {
	return e.cli.VolumeRemove(ctx, name, true) // force
}

func (e *dockerEngine) ServiceScale(ctx context.Context, name string, replicas uint64) error {
	cur, found, err := e.findService(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return nil // nothing to scale
	}
	spec := cur.Spec
	if spec.Mode.Replicated == nil {
		spec.Mode.Replicated = &swarm.ReplicatedService{}
	}
	spec.Mode.Replicated.Replicas = &replicas
	_, err = e.cli.ServiceUpdate(ctx, cur.ID, cur.Version, spec, swarm.ServiceUpdateOptions{})
	return err
}
