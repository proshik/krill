package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
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
	createOpts := swarm.ServiceCreateOptions{}
	updateOpts := swarm.ServiceUpdateOptions{}
	if spec.RegistryAuth != "" {
		createOpts.EncodedRegistryAuth = spec.RegistryAuth
		updateOpts.EncodedRegistryAuth = spec.RegistryAuth
	}
	if !found {
		_, err = e.cli.ServiceCreate(ctx, sw, createOpts)
		return err
	}
	// ForceUpdate+1 — so that even an unchanged tag triggers a rolling-update (redeploy).
	sw.TaskTemplate.ForceUpdate = cur.Spec.TaskTemplate.ForceUpdate + 1
	_, err = e.cli.ServiceUpdate(ctx, cur.ID, cur.Version, sw, updateOpts)
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
	running, failed := 0, 0
	for _, t := range tasks {
		switch t.Status.State {
		case swarm.TaskStateRunning:
			running++
		case swarm.TaskStateFailed, swarm.TaskStateRejected:
			failed++
		}
	}
	return ServiceState{Found: true, Running: running, Desired: desired, Failed: failed}, nil
}

// ServiceStates returns the state of many services in ONE ServiceList + ONE
// TaskList (instead of a call per service) — used to render status badges for a
// page full of cards without N round-trips. Missing services are absent from
// the map (caller falls back to the stored status).
func (e *dockerEngine) ServiceStates(ctx context.Context, names []string) (map[string]ServiceState, error) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	svcs, err := e.cli.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	idToName := map[string]string{}
	out := map[string]ServiceState{}
	for _, s := range svcs {
		if !want[s.Spec.Name] {
			continue
		}
		idToName[s.ID] = s.Spec.Name
		desired := 0
		if r := s.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
			desired = int(*r.Replicas)
		}
		out[s.Spec.Name] = ServiceState{Found: true, Desired: desired}
	}
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("desired-state", "running")),
	})
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		name, ok := idToName[t.ServiceID]
		if !ok {
			continue
		}
		st := out[name]
		switch t.Status.State {
		case swarm.TaskStateRunning:
			st.Running++
		case swarm.TaskStateFailed, swarm.TaskStateRejected:
			st.Failed++
		}
		out[name] = st
	}
	return out, nil
}

func (e *dockerEngine) ServiceLogs(ctx context.Context, name string, follow bool) (io.ReadCloser, error) {
	rc, err := e.cli.ServiceLogs(ctx, name, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       "200",
		Timestamps: true,
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

func (e *dockerEngine) ServiceUpdateLabels(ctx context.Context, name string, labels map[string]string) error {
	cur, found, err := e.findService(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return nil // not deployed yet; labels apply on next deploy
	}
	spec := cur.Spec
	spec.Annotations.Labels = labels
	_, err = e.cli.ServiceUpdate(ctx, cur.ID, cur.Version, spec, swarm.ServiceUpdateOptions{})
	return err
}

// runningContainerID returns the container ID of a running task of serviceName.
func (e *dockerEngine) runningContainerID(ctx context.Context, serviceName string) (string, error) {
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", serviceName),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return "", err
	}
	for _, t := range tasks {
		if t.Status.ContainerStatus != nil && t.Status.ContainerStatus.ContainerID != "" {
			return t.Status.ContainerStatus.ContainerID, nil
		}
	}
	return "", fmt.Errorf("no running container for service %s", serviceName)
}

// dockerExecSession adapts a hijacked exec attach to ExecSession.
type dockerExecSession struct {
	cli    *client.Client
	att    types.HijackedResponse
	execID string
}

func (s *dockerExecSession) Read(p []byte) (int, error)  { return s.att.Reader.Read(p) }
func (s *dockerExecSession) Write(p []byte) (int, error) { return s.att.Conn.Write(p) }
func (s *dockerExecSession) Resize(ctx context.Context, rows, cols uint) error {
	return s.cli.ContainerExecResize(ctx, s.execID, container.ResizeOptions{Height: rows, Width: cols})
}
func (s *dockerExecSession) Close() error { s.att.Close(); return nil }

// ExecInteractive starts an interactive TTY exec of cmd in a running container.
func (e *dockerEngine) ExecInteractive(ctx context.Context, serviceName string, cmd []string) (ExecSession, error) {
	containerID, err := e.runningContainerID(ctx, serviceName)
	if err != nil {
		return nil, err
	}
	idResp, err := e.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, err
	}
	att, err := e.cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, err
	}
	return &dockerExecSession{cli: e.cli, att: att, execID: idResp.ID}, nil
}

func (e *dockerEngine) Exec(ctx context.Context, serviceName string, cmd []string, env []string, stdin io.Reader, stdout io.Writer) error {
	containerID, err := e.runningContainerID(ctx, serviceName)
	if err != nil {
		return err
	}
	idResp, err := e.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  stdin != nil,
	})
	if err != nil {
		return err
	}
	att, err := e.cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer att.Close()
	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			_ = att.CloseWrite()
		}()
	}
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(stdout, &stderr, att.Reader); err != nil {
		return err
	}
	insp, err := e.cli.ContainerExecInspect(ctx, idResp.ID)
	if err != nil {
		return err
	}
	if insp.ExitCode != 0 {
		return fmt.Errorf("exec %v exited %d: %s", cmd, insp.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (e *dockerEngine) RegistryCheck(ctx context.Context, serverAddr, username, password string) error {
	_, err := e.cli.RegistryLogin(ctx, registry.AuthConfig{Username: username, Password: password, ServerAddress: serverAddr})
	return err
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

// ServiceRestart forces a zero-downtime rolling restart of the existing service
// with its current spec (no rebuild/re-pull). If the service was stopped
// (0 replicas) it is brought back to 1.
func (e *dockerEngine) ServiceRestart(ctx context.Context, name string) error {
	cur, found, err := e.findService(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return nil // nothing to restart
	}
	spec := cur.Spec
	spec.TaskTemplate.ForceUpdate = cur.Spec.TaskTemplate.ForceUpdate + 1
	if spec.Mode.Replicated != nil && (spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas == 0) {
		one := uint64(1)
		spec.Mode.Replicated.Replicas = &one
	}
	_, err = e.cli.ServiceUpdate(ctx, cur.ID, cur.Version, spec, swarm.ServiceUpdateOptions{})
	return err
}

// cpuPercent computes docker-style %CPU (0..onlineCPUs*100).
func cpuPercent(cpuDelta, systemDelta uint64, onlineCPUs uint32) float64 {
	if cpuDelta == 0 || systemDelta == 0 {
		return 0
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}
	return (float64(cpuDelta) / float64(systemDelta)) * float64(onlineCPUs) * 100.0
}

// workingSet approximates the "working set" memory like `docker stats` does:
// usage minus the page cache (cgroup v2 reports it as inactive_file, v1 as
// total_inactive_file). Avoids the inflated raw Usage figure.
func workingSet(m container.MemoryStats) uint64 {
	cache := m.Stats["inactive_file"]
	if cache == 0 {
		cache = m.Stats["total_inactive_file"]
	}
	if cache > 0 && cache <= m.Usage {
		return m.Usage - cache
	}
	return m.Usage
}

func (e *dockerEngine) NodeInfo(ctx context.Context) (NodeInfo, error) {
	info, err := e.cli.Info(ctx)
	if err != nil {
		return NodeInfo{}, err
	}
	return NodeInfo{MemTotal: info.MemTotal, NCPU: info.NCPU}, nil
}

// cpuSampleWindow is the gap between the two stats reads used to derive a
// point-in-time CPU%. One-shot stats do not prime PreCPUStats, so a single read
// cannot yield a delta — we read twice and diff (the same way `docker stats` does).
const cpuSampleWindow = time.Second

// statsOneShot reads a single container's stats snapshot.
func (e *dockerEngine) statsOneShot(ctx context.Context, id string) (container.StatsResponse, bool) {
	resp, err := e.cli.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return container.StatsResponse{}, false
	}
	defer resp.Body.Close()
	var s container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return container.StatsResponse{}, false
	}
	return s, true
}

func (e *dockerEngine) ListContainerStats(ctx context.Context) ([]ContainerStat, error) {
	hostname, _ := os.Hostname() // in a container this is the short container id
	ctrs, err := e.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}
	// First reading of the cumulative CPU counters per container.
	prev := make([]container.StatsResponse, len(ctrs))
	prevOK := make([]bool, len(ctrs))
	for i, c := range ctrs {
		prev[i], prevOK[i] = e.statsOneShot(ctx, c.ID)
	}
	// Wait so the second reading reflects CPU consumed in between.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(cpuSampleWindow):
	}
	out := make([]ContainerStat, 0, len(ctrs))
	for i, c := range ctrs {
		s, ok := e.statsOneShot(ctx, c.ID)
		if !ok {
			continue
		}
		online := s.CPUStats.OnlineCPUs
		if online == 0 {
			online = uint32(len(s.CPUStats.CPUUsage.PercpuUsage))
		}
		var cpu float64
		if prevOK[i] {
			cpu = cpuPercent(
				s.CPUStats.CPUUsage.TotalUsage-prev[i].CPUStats.CPUUsage.TotalUsage,
				s.CPUStats.SystemUsage-prev[i].CPUStats.SystemUsage,
				online,
			)
		}
		comp := c.Labels["com.docker.swarm.service.name"]
		if comp == "" && len(c.Names) > 0 {
			comp = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ContainerStat{
			Component:     comp,
			CPUPct:        cpu,
			MemBytes:      int64(workingSet(s.MemoryStats)),
			MemLimitBytes: int64(s.MemoryStats.Limit),
			SelfControl:   hostname != "" && strings.HasPrefix(c.ID, hostname),
		})
	}
	return out, nil
}
