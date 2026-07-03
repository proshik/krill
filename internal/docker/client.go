package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type cpuCounters struct{ total, system uint64 }

// CPUCache caches each container's previous cumulative CPU counters so a single
// one-shot read can yield a delta across sampler ticks. One instance per daemon
// connection (so identical container IDs on different nodes never mix).
type CPUCache struct {
	mu   sync.Mutex
	prev map[string]cpuCounters
}

func NewCPUCache() *CPUCache { return &CPUCache{prev: map[string]cpuCounters{}} }

// delta returns docker-style %CPU for id given the current counters; 0 on the
// first sample or after a counter reset (container restart).
func (c *CPUCache) delta(id string, cur cpuCounters, online uint32) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var pct float64
	if prev, had := c.prev[id]; had && cur.total >= prev.total && cur.system >= prev.system {
		pct = cpuPercent(cur.total-prev.total, cur.system-prev.system, online)
	}
	c.prev[id] = cur
	return pct
}

// retain drops counters for containers no longer present (bounded growth).
func (c *CPUCache) retain(seen map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.prev {
		if !seen[id] {
			delete(c.prev, id)
		}
	}
}

// statsCollector holds a docker client + its dedicated CPU cache and implements
// the ListContainerStats / NodeInfo logic against any *client.Client — local or
// SSH-tunnelled.
type statsCollector struct {
	cli   *client.Client
	cache *CPUCache
}

func (sc *statsCollector) listContainerStats(ctx context.Context) ([]ContainerStat, error) {
	// For a remote (SSH-tunnelled) node this is the control-plane hostname, so
	// SelfControl is correctly always false for worker containers (Krill never
	// runs on workers).
	hostname, _ := os.Hostname()
	ctrs, err := sc.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(ctrs))
	out := make([]ContainerStat, 0, len(ctrs))
	for _, c := range ctrs {
		s, ok := statsOneShot(ctx, sc.cli, c.ID)
		if !ok {
			continue
		}
		seen[c.ID] = true
		online := s.CPUStats.OnlineCPUs
		if online == 0 {
			online = uint32(len(s.CPUStats.CPUUsage.PercpuUsage))
		}
		cur := cpuCounters{total: s.CPUStats.CPUUsage.TotalUsage, system: s.CPUStats.SystemUsage}
		cpu := sc.cache.delta(c.ID, cur, online)
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
	sc.cache.retain(seen)
	return out, nil
}

func (sc *statsCollector) nodeInfo(ctx context.Context) (NodeInfo, error) {
	info, err := sc.cli.Info(ctx)
	if err != nil {
		return NodeInfo{}, err
	}
	return NodeInfo{MemTotal: info.MemTotal, NCPU: info.NCPU}, nil
}

// RemoteStats samples a docker daemon reached over a custom dialer (e.g. a
// worker's unix socket forwarded through SSH). It owns its own CPU cache.
type RemoteStats struct {
	cli *client.Client
	sc  *statsCollector
}

// NewRemoteStats builds a docker client whose connections come from dial. The
// dialer's address is the remote unix socket; callers (the metrics sampler)
// supply a dialer that opens the worker's /var/run/docker.sock over SSH.
func NewRemoteStats(dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*RemoteStats, error) {
	cli, err := client.NewClientWithOpts(
		// WithHost MUST come before WithDialContext: for a unix host WithHost
		// calls sockets.ConfigureTransport which sets a local-socket DialContext,
		// overwriting ours. Set the tunnel dialer last so it wins — otherwise the
		// "remote" client silently talks to the LOCAL daemon.
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithDialContext(dial),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	return &RemoteStats{cli: cli, sc: &statsCollector{cli: cli, cache: NewCPUCache()}}, nil
}

func (r *RemoteStats) ListContainerStats(ctx context.Context) ([]ContainerStat, error) {
	return r.sc.listContainerStats(ctx)
}
func (r *RemoteStats) NodeInfo(ctx context.Context) (NodeInfo, error) { return r.sc.nodeInfo(ctx) }
func (r *RemoteStats) Close() error                                   { return r.cli.Close() }

// RemoteClientProvider returns a docker client for the daemon of the node a
// container runs on. Contract: (nil,nil,nil) => the node is local (control
// plane / not a registered worker); the caller uses the local client.
// (cli,closeFn,nil) => a worker reached over SSH; the caller MUST call closeFn.
// (nil,nil,err) => the node is unreachable; the exec must fail.
type RemoteClientProvider func(ctx context.Context, nodeID string) (cli *client.Client, closeFn func() error, err error)

// NewRemoteClient builds a full docker client whose connections come from dial
// (e.g. a worker's /var/run/docker.sock forwarded over SSH). Mirrors
// NewRemoteStats but returns the general client for exec.
func NewRemoteClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*client.Client, error) {
	return client.NewClientWithOpts(
		// WithHost before WithDialContext (see NewRemoteStats): a unix host's
		// WithHost overwrites transport.DialContext, so the tunnel dialer must be
		// set last or the client silently talks to the LOCAL daemon.
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithDialContext(dial),
		client.WithAPIVersionNegotiation(),
	)
}

// selectClient picks the docker client for a container's node: the local client
// when there is no provider or the provider reports a local node, otherwise the
// remote client (whose release closes the SSH tunnel). release is always
// non-nil and safe to call.
func selectClient(ctx context.Context, local *client.Client, provider RemoteClientProvider, nodeID string) (cli *client.Client, release func(), err error) {
	if provider == nil {
		return local, func() {}, nil
	}
	rcli, closeFn, perr := provider(ctx, nodeID)
	if perr != nil {
		return nil, func() {}, perr
	}
	if rcli == nil {
		return local, func() {}, nil
	}
	return rcli, func() { _ = closeFn() }, nil
}

// SetRemoteClientProvider injects the node→docker-client provider (wired in
// main.go). Nil (default) means every exec runs against the local daemon.
func (e *dockerEngine) SetRemoteClientProvider(p RemoteClientProvider) { e.remoteProvider = p }

// clientForContainer resolves serviceName's running container and returns the
// docker client for the node it runs on (local or remote-over-SSH). release
// must always be called (no-op for local).
func (e *dockerEngine) clientForContainer(ctx context.Context, serviceName string) (cli *client.Client, containerID string, release func(), err error) {
	containerID, nodeID, err := e.runningContainerID(ctx, serviceName)
	if err != nil {
		return nil, "", func() {}, err
	}
	cli, release, err = selectClient(ctx, e.cli, e.remoteProvider, nodeID)
	if err != nil {
		return nil, "", func() {}, fmt.Errorf("reach node for service %s: %w", serviceName, err)
	}
	return cli, containerID, release, nil
}

type dockerEngine struct {
	cli            *client.Client
	collector      *statsCollector
	remoteProvider RemoteClientProvider
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
	eng := &dockerEngine{cli: cli, collector: &statsCollector{cli: cli, cache: NewCPUCache()}}
	return eng, nil
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
	// Swarm forbids changing a service's mode in place (replicated <-> global):
	// ServiceUpdate returns "service mode change is not allowed". When the
	// desired mode differs from the running one, recreate the service (remove +
	// create) under the same name instead of updating it.
	if (cur.Spec.Mode.Global != nil) != (sw.Mode.Global != nil) {
		return e.recreateForModeChange(ctx, cur.ID, sw, createOpts)
	}
	// ForceUpdate+1 — so that even an unchanged tag triggers a rolling-update (redeploy).
	sw.TaskTemplate.ForceUpdate = cur.Spec.TaskTemplate.ForceUpdate + 1
	_, err = e.cli.ServiceUpdate(ctx, cur.ID, cur.Version, sw, updateOpts)
	return err
}

// recreateForModeChange removes the existing service and recreates it under the
// same name. Used when the service mode (replicated/global) changes, which
// Swarm cannot do in place. The service name frees up once the removal is
// processed, so ServiceCreate is retried briefly on a transient name conflict.
func (e *dockerEngine) recreateForModeChange(ctx context.Context, id string, sw swarm.ServiceSpec, createOpts swarm.ServiceCreateOptions) error {
	if err := e.cli.ServiceRemove(ctx, id); err != nil {
		return fmt.Errorf("remove for mode change: %w", err)
	}
	var cerr error
	for i := 0; i < 15; i++ {
		if _, cerr = e.cli.ServiceCreate(ctx, sw, createOpts); cerr == nil {
			return nil
		}
		low := strings.ToLower(cerr.Error())
		if !strings.Contains(low, "name conflict") && !strings.Contains(low, "already in use") && !strings.Contains(low, "already exists") {
			return cerr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("recreate for mode change: %w", cerr)
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
	// A global service has no Replicas; Swarm wants one task per matching node,
	// so the desired-state=running task count IS the desired count.
	if svc.Spec.Mode.Global != nil {
		desired = len(tasks)
	}
	return ServiceState{Found: true, Running: running, Desired: desired, Failed: failed}, nil
}

func (e *dockerEngine) ServiceProgress(ctx context.Context, name string, exclude []string) (ServiceProgress, error) {
	svc, found, err := e.findService(ctx, name)
	if err != nil {
		return ServiceProgress{}, err
	}
	if !found {
		return ServiceProgress{Found: false}, nil
	}
	desired := 0
	if r := svc.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
		desired = int(*r.Replicas)
	}
	updateState := ""
	if svc.UpdateStatus != nil {
		updateState = string(svc.UpdateStatus.State)
	}
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", name),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return ServiceProgress{}, err
	}
	old := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		old[id] = true
	}
	running, failed, fresh := 0, 0, 0
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
		// Only tasks NOT in the pre-deploy baseline count: during a StartFirst
		// rolling update the old task (same ID) stays desired-state=running until
		// the new one is ready and must not satisfy convergence.
		if old[t.ID] {
			continue
		}
		fresh++
		switch t.Status.State {
		case swarm.TaskStateRunning:
			running++
		case swarm.TaskStateFailed, swarm.TaskStateRejected:
			failed++
		}
	}
	// A global service has no Replicas; Swarm schedules one task per matching
	// node. The desired count for this deploy is the number of new (non-baseline)
	// tasks Swarm created — 0 until they appear, so convergence keeps waiting
	// rather than falsely succeeding off the old tasks.
	if svc.Spec.Mode.Global != nil {
		desired = fresh
	}
	return ServiceProgress{
		Found: true, Desired: desired, Running: running, Failed: failed,
		UpdateState: updateState, TaskIDs: ids,
	}, nil
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
	global := map[string]bool{}
	for _, s := range svcs {
		if !want[s.Spec.Name] {
			continue
		}
		idToName[s.ID] = s.Spec.Name
		desired := 0
		if r := s.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
			desired = int(*r.Replicas)
		}
		if s.Spec.Mode.Global != nil {
			global[s.Spec.Name] = true
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
		// A global service has no Replicas; its desired count is the number of
		// desired-state=running tasks (one per matching node).
		if global[name] {
			st.Desired++
		}
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

// busyboxImage is the pinned helper used to tar/untar app volumes. App images
// often lack tar (e.g. Readeck), so we run a sidecar with the volume mounted.
// Pinned by digest — never :latest. Bump only with a security review.
const busyboxImage = "busybox:1.37.0@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"

// ensureImage pulls ref if no local image matches it.
func (e *dockerEngine) ensureImage(ctx context.Context, ref string) error {
	return ensureImageOn(ctx, e.cli, ref)
}

// ensureImageOn pulls ref on the given client if it has no matching local image.
func ensureImageOn(ctx context.Context, cli *client.Client, ref string) error {
	imgs, err := cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", ref)),
	})
	if err == nil && len(imgs) > 0 {
		return nil
	}
	rc, perr := cli.ImagePull(ctx, ref, image.PullOptions{})
	if perr != nil {
		return perr
	}
	defer rc.Close()
	_, perr = io.Copy(io.Discard, rc)
	return perr
}

// VolumeArchive streams a tar of the named volume's contents to out (raw tar;
// the caller gzips). Runs busybox as root with the volume mounted read-only and
// no network. Root is required so files owned by any uid (apps run as varied
// non-root users) are readable, and so original ownership is captured in the tar.
func (e *dockerEngine) VolumeArchive(ctx context.Context, volumeName string, out io.Writer) error {
	if err := e.ensureImage(ctx, busyboxImage); err != nil {
		return err
	}
	resp, err := e.cli.ContainerCreate(ctx,
		&container.Config{
			Image: busyboxImage,
			Cmd:   []string{"tar", "-c", "-C", "/vol", "."},
		},
		&container.HostConfig{
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/vol", ReadOnly: true}},
			NetworkMode: "none",
		}, nil, nil, "")
	if err != nil {
		return err
	}
	cid := resp.ID
	defer e.cli.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true})

	att, err := e.cli.ContainerAttach(ctx, cid, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return err
	}
	defer att.Close()
	if err := e.cli.ContainerStart(ctx, cid, container.StartOptions{}); err != nil {
		return err
	}
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(out, &stderr, att.Reader); err != nil {
		return err
	}
	return waitContainer(ctx, e.cli, cid, "volume archive "+volumeName, &stderr)
}

// VolumeRestore extracts a tar (read from in; the caller gunzips) into the named
// volume. Mounts the volume read-write; busybox runs as root with no network.
// Root is required to write into the (root-owned) fresh-volume root and to
// restore each entry's original ownership (tar's default) so the app can read
// its data back as whatever uid it runs under.
func (e *dockerEngine) VolumeRestore(ctx context.Context, volumeName string, in io.Reader) error {
	if err := e.ensureImage(ctx, busyboxImage); err != nil {
		return err
	}
	resp, err := e.cli.ContainerCreate(ctx,
		&container.Config{
			Image:     busyboxImage,
			Cmd:       []string{"tar", "-x", "-C", "/vol"},
			OpenStdin: true, StdinOnce: true,
		},
		&container.HostConfig{
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/vol"}},
			NetworkMode: "none",
		}, nil, nil, "")
	if err != nil {
		return err
	}
	cid := resp.ID
	defer e.cli.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true})

	att, err := e.cli.ContainerAttach(ctx, cid, container.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return err
	}
	defer att.Close()
	if err := e.cli.ContainerStart(ctx, cid, container.StartOptions{}); err != nil {
		return err
	}
	copyErr := make(chan error, 1)
	go func() {
		_, ce := io.Copy(att.Conn, in)
		_ = att.CloseWrite()
		copyErr <- ce
	}()
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(io.Discard, &stderr, att.Reader); err != nil {
		return err
	}
	if err := waitContainer(ctx, e.cli, cid, "volume restore "+volumeName, &stderr); err != nil {
		return err
	}
	if ce := <-copyErr; ce != nil {
		return fmt.Errorf("stream archive to %s: %w", volumeName, ce)
	}
	return nil
}

// VolumeChown runs a short root busybox sidecar that chowns a named volume's
// contents to uid:gid. It runs on the node given by swarmNodeID: an empty id (or
// a node that is not a registered worker → the control plane) uses the local
// daemon; a registered worker is reached over the SSH tunnel (via the same
// selectClient path as Exec). Fresh named volumes are created root:root 0755, so
// a non-root app cannot write until this runs (the deployer runs it before
// ServiceDeploy). chown -R is instant on a fresh empty volume.
func (e *dockerEngine) VolumeChown(ctx context.Context, volumeName string, uid, gid int, swarmNodeID string) error {
	cli, release, err := selectClient(ctx, e.cli, e.remoteProvider, swarmNodeID)
	if err != nil {
		return fmt.Errorf("reach node for volume chown %s: %w", volumeName, err)
	}
	defer release()
	if err := ensureImageOn(ctx, cli, busyboxImage); err != nil {
		return err
	}
	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: busyboxImage,
			Cmd:   []string{"chown", "-R", fmt.Sprintf("%d:%d", uid, gid), "/vol"},
		},
		&container.HostConfig{
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/vol"}},
			NetworkMode: "none",
		}, nil, nil, "")
	if err != nil {
		return err
	}
	cid := resp.ID
	defer cli.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true})

	att, err := cli.ContainerAttach(ctx, cid, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return err
	}
	defer att.Close()
	if err := cli.ContainerStart(ctx, cid, container.StartOptions{}); err != nil {
		return err
	}
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(io.Discard, &stderr, att.Reader); err != nil {
		return err
	}
	return waitContainer(ctx, cli, cid, "volume chown "+volumeName, &stderr)
}

// waitContainer blocks until the container exits and returns an error on a
// non-zero exit code (including the captured stderr).
func waitContainer(ctx context.Context, cli *client.Client, cid, what string, stderr *bytes.Buffer) error {
	statusCh, errCh := cli.ContainerWait(ctx, cid, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return err
	case st := <-statusCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("%s exited %d: %s", what, st.StatusCode, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
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

// runningContainerID returns the container ID of a running task of serviceName,
// along with the ID of the node that task is running on (for exec routing).
func (e *dockerEngine) runningContainerID(ctx context.Context, serviceName string) (containerID, nodeID string, err error) {
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", serviceName),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return "", "", err
	}
	for _, t := range tasks {
		if t.Status.ContainerStatus != nil && t.Status.ContainerStatus.ContainerID != "" {
			return t.Status.ContainerStatus.ContainerID, t.NodeID, nil
		}
	}
	return "", "", fmt.Errorf("no running container for service %s", serviceName)
}

// dockerExecSession adapts a hijacked exec attach to ExecSession.
type dockerExecSession struct {
	cli     *client.Client
	att     types.HijackedResponse
	execID  string
	release func() // tears down the SSH tunnel; no-op for a local session
}

func (s *dockerExecSession) Read(p []byte) (int, error)  { return s.att.Reader.Read(p) }
func (s *dockerExecSession) Write(p []byte) (int, error) { return s.att.Conn.Write(p) }
func (s *dockerExecSession) Resize(ctx context.Context, rows, cols uint) error {
	return s.cli.ContainerExecResize(ctx, s.execID, container.ResizeOptions{Height: rows, Width: cols})
}
func (s *dockerExecSession) Close() error {
	s.att.Close()
	if s.release != nil {
		s.release()
	}
	return nil
}

// ExecInteractive starts an interactive TTY exec of cmd in a running container.
func (e *dockerEngine) ExecInteractive(ctx context.Context, serviceName string, cmd []string) (ExecSession, error) {
	cli, containerID, release, err := e.clientForContainer(ctx, serviceName)
	if err != nil {
		return nil, err
	}
	idResp, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		release()
		return nil, err
	}
	att, err := cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		release()
		return nil, err
	}
	return &dockerExecSession{cli: cli, att: att, execID: idResp.ID, release: release}, nil
}

func (e *dockerEngine) Exec(ctx context.Context, serviceName string, cmd []string, env []string, stdin io.Reader, stdout io.Writer) error {
	cli, containerID, release, err := e.clientForContainer(ctx, serviceName)
	if err != nil {
		return err
	}
	defer release()
	idResp, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  stdin != nil,
	})
	if err != nil {
		return err
	}
	att, err := cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{})
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
	insp, err := cli.ContainerExecInspect(ctx, idResp.ID)
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
	// A global service has no replica count, and Swarm forbids changing a
	// service's mode in place — so it cannot be scaled via ServiceUpdate at all.
	// "Scaling to 0" (Stop) therefore means removing it; the next Deploy
	// recreates it from the stored spec. (Scaling a global service up is not a
	// real path — apps are restarted via Deploy, and managed DBs are never global.)
	if cur.Spec.Mode.Global != nil {
		if replicas == 0 {
			return e.cli.ServiceRemove(ctx, cur.ID)
		}
		spec := cur.Spec
		spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}
		return e.recreateForModeChange(ctx, cur.ID, spec, swarm.ServiceCreateOptions{})
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
	return e.collector.nodeInfo(ctx)
}

func (e *dockerEngine) Nodes(ctx context.Context) ([]SwarmNode, error) {
	ns, err := e.cli.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]SwarmNode, 0, len(ns))
	for _, n := range ns {
		out = append(out, SwarmNode{
			ID: n.ID, Hostname: n.Description.Hostname, Role: string(n.Spec.Role),
			Availability: string(n.Spec.Availability), State: string(n.Status.State),
			Addr: n.Status.Addr, Leader: n.ManagerStatus != nil && n.ManagerStatus.Leader,
		})
	}
	return out, nil
}

func (e *dockerEngine) NodeSetAvailability(ctx context.Context, nodeID, availability string) error {
	n, _, err := e.cli.NodeInspectWithRaw(ctx, nodeID)
	if err != nil {
		return err
	}
	n.Spec.Availability = swarm.NodeAvailability(availability)
	return e.cli.NodeUpdate(ctx, nodeID, n.Version, n.Spec)
}

func (e *dockerEngine) NodeRemove(ctx context.Context, nodeID string, force bool) error {
	return e.cli.NodeRemove(ctx, nodeID, swarm.NodeRemoveOptions{Force: force})
}

func (e *dockerEngine) SwarmWorkerToken(ctx context.Context) (string, error) {
	sw, err := e.cli.SwarmInspect(ctx)
	if err != nil {
		return "", err
	}
	return sw.JoinTokens.Worker, nil
}

func (e *dockerEngine) ServiceTasks(ctx context.Context, name string) ([]TaskPlacement, error) {
	// desired-state=running (like the other TaskList calls) so terminated tasks
	// that Swarm keeps in history are not reported as live placements.
	tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", name),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return nil, err
	}
	id2name := map[string]string{}
	if nodes, nerr := e.Nodes(ctx); nerr == nil { // degrade to raw IDs if unresolved
		for _, n := range nodes {
			id2name[n.ID] = n.Hostname
		}
	}
	out := make([]TaskPlacement, 0, len(tasks))
	for _, t := range tasks {
		nodeName := id2name[t.NodeID]
		if nodeName == "" {
			nodeName = t.NodeID // fall back to the raw node ID; "" if not yet scheduled
		}
		out = append(out, TaskPlacement{
			NodeID: t.NodeID, NodeName: nodeName,
			State: string(t.Status.State), Desired: string(t.DesiredState),
		})
	}
	return out, nil
}

func (e *dockerEngine) NodeSetLabel(ctx context.Context, nodeID, key, value string) error {
	n, _, err := e.cli.NodeInspectWithRaw(ctx, nodeID)
	if err != nil {
		return err
	}
	if n.Spec.Annotations.Labels == nil {
		n.Spec.Annotations.Labels = map[string]string{}
	}
	if n.Spec.Annotations.Labels[key] == value {
		return nil // already set — avoid a no-op version bump
	}
	n.Spec.Annotations.Labels[key] = value
	return e.cli.NodeUpdate(ctx, nodeID, n.Version, n.Spec)
}

func (e *dockerEngine) NodeDeleteLabel(ctx context.Context, nodeID, key string) error {
	n, _, err := e.cli.NodeInspectWithRaw(ctx, nodeID)
	if err != nil {
		return err
	}
	if _, ok := n.Spec.Annotations.Labels[key]; !ok {
		return nil // absent — nothing to do
	}
	delete(n.Spec.Annotations.Labels, key)
	return e.cli.NodeUpdate(ctx, nodeID, n.Version, n.Spec)
}

// ResolveDigest queries the registry for the current digest of ref (no pull)
// and returns a digest-pinned reference of the form repo@sha256:…. On any
// error the caller falls back to the plain tag. encodedAuth is the same
// X-Registry-Auth blob used for ServiceCreate/Update; pass "" for public images.
func (e *dockerEngine) ResolveDigest(ctx context.Context, ref, encodedAuth string) (string, error) {
	di, err := e.cli.DistributionInspect(ctx, ref, encodedAuth)
	if err != nil {
		return "", err
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", err
	}
	canonical, err := reference.WithDigest(reference.TrimNamed(named), di.Descriptor.Digest)
	if err != nil {
		return "", err
	}
	return reference.FamiliarString(canonical), nil
}

// statsOneShot reads a single container's stats snapshot against the given client.
func statsOneShot(ctx context.Context, cli *client.Client, id string) (container.StatsResponse, bool) {
	resp, err := cli.ContainerStatsOneShot(ctx, id)
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

// ListContainerStats samples each running container once and derives CPU% from
// the delta against the PREVIOUS sample (cached per container ID across calls).
// Delegates to the engine's statsCollector.
func (e *dockerEngine) ListContainerStats(ctx context.Context) ([]ContainerStat, error) {
	return e.collector.listContainerStats(ctx)
}
