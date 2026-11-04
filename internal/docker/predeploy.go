package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

const predeployLabel = "krill.predeploy"

// PredeployRunner is optional: an engine without it can deploy ordinary apps,
// but must refuse a configured predeploy command instead of silently skipping it.
type PredeployRunner interface {
	LocalImageID(context.Context, string) (string, error)
	RunPredeploy(context.Context, PredeploySpec, io.Writer) error
	CleanupPredeploy(context.Context) error
}

type PredeploySpec struct {
	AppID, DeploymentID                   int64
	Image, RegistryAuth, Command, Network string
	Env                                   map[string]string
	PlacementMode                         string
	PlacementNodes                        []string
	MemoryLimitBytes, NanoCPUs            int64
}

func (e *dockerEngine) LocalImageID(ctx context.Context, ref string) (string, error) {
	img, _, err := e.cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return "", err
	}
	return img.ID, nil
}

// ChoosePredeployNode picks one eligible node once; global services still run
// only one migration. Explicit node IDs avoid mutable placement-label races.
func ChoosePredeployNode(nodes []SwarmNode, mode string, selected []string) (string, error) {
	allowed := map[string]bool{}
	for _, id := range selected {
		allowed[id] = true
	}
	pinned := mode == "pin" || mode == "global"
	candidates := []SwarmNode{}
	for _, n := range nodes {
		if n.State != "ready" || n.Availability != "active" || (pinned && !allowed[n.ID]) {
			continue
		}
		candidates = append(candidates, n)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !pinned && (candidates[i].Role == "manager") != (candidates[j].Role == "manager") {
			return candidates[i].Role == "manager"
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(candidates) == 0 {
		return "", errors.New("no ready active node available for predeploy")
	}
	return candidates[0].ID, nil
}

func (e *dockerEngine) RunPredeploy(ctx context.Context, s PredeploySpec, out io.Writer) (retErr error) {
	nodes, err := e.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("list predeploy nodes: %w", err)
	}
	node, err := ChoosePredeployNode(nodes, s.PlacementMode, s.PlacementNodes)
	if err != nil {
		return err
	}
	cli, release, err := e.verifiedNodeClient(ctx, node)
	if err != nil {
		return err
	}
	defer release()
	if !IsImmutableImage(s.Image) {
		return errors.New("predeploy requires an immutable image")
	}
	if _, _, err = cli.ImageInspectWithRaw(ctx, s.Image); err != nil {
		if !errdefs.IsNotFound(err) || strings.HasPrefix(s.Image, "sha256:") {
			return fmt.Errorf("inspect predeploy image: %w", err)
		}
		pull, err := cli.ImagePull(ctx, s.Image, image.PullOptions{RegistryAuth: s.RegistryAuth})
		if err != nil {
			return fmt.Errorf("pull predeploy image: %w", err)
		}
		_, copyErr := io.Copy(io.Discard, pull)
		closeErr := pull.Close()
		if copyErr != nil {
			return fmt.Errorf("pull predeploy image: %w", copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		// Pull can return an HTTP 200 JSON error stream. Inspect is the proof.
		if _, _, err = cli.ImageInspectWithRaw(ctx, s.Image); err != nil {
			return fmt.Errorf("verify predeploy image: %w", err)
		}
	}
	env := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	name := fmt.Sprintf("krill-predeploy-%d-%d", s.AppID, s.DeploymentID)
	// Creation may succeed even if its response is lost. Register cleanup first,
	// resolve by deterministic name, and prove ownership before deleting by ID.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cleanupClient, releaseCleanup, err := e.verifiedNodeClient(cleanupCtx, node)
		if err == nil {
			defer releaseCleanup()
			var owned container.InspectResponse
			owned, err = cleanupClient.ContainerInspect(cleanupCtx, name)
			if err == nil && owned.Config != nil && owned.Config.Labels[predeployLabel] == "1" &&
				owned.Config.Labels["krill.predeploy.app"] == fmt.Sprint(s.AppID) && owned.Config.Labels["krill.predeploy.deployment"] == fmt.Sprint(s.DeploymentID) {
				err = cleanupClient.ContainerRemove(cleanupCtx, owned.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
			}
		}
		if err != nil && !errdefs.IsNotFound(err) {
			retErr = errors.Join(retErr, fmt.Errorf("remove predeploy container %s on node %s: %w", name, node, err))
		}
	}()
	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: s.Image, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{s.Command},
		Env: env, Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		Labels: map[string]string{predeployLabel: "1", "krill.predeploy.app": fmt.Sprint(s.AppID), "krill.predeploy.deployment": fmt.Sprint(s.DeploymentID)},
	}, &container.HostConfig{
		NetworkMode: container.NetworkMode(s.Network), RestartPolicy: container.RestartPolicy{Name: "no"},
		// Output is consumed only through attach and masked by the deployer.
		// Do not retain an unmasked second copy for Docker log readers/collectors.
		LogConfig: container.LogConfig{Type: "none"},
		Resources: container.Resources{Memory: s.MemoryLimitBytes, NanoCPUs: s.NanoCPUs},
	}, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{s.Network: {}}}, nil,
		name)
	if err != nil {
		return fmt.Errorf("create predeploy container: %w", err)
	}
	// ContainerAttach's HTTP upgrade does raw socket IO after dialing. Give it
	// a scoped transport whose owned connection is closed even during handshake.
	attachClient, err := predeployAttachClient(ctx, cli)
	if err != nil {
		return err
	}
	defer attachClient.Close()
	att, err := attachClient.ContainerAttach(ctx, resp.ID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return fmt.Errorf("attach predeploy logs: %w", err)
	}
	defer att.Close()
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start predeploy container: %w", err)
	}
	logsDone := make(chan struct{})
	var logErr error
	go func() { _, logErr = stdcopy.StdCopy(out, out, att.Reader); close(logsDone) }()
	defer func() { att.Close(); <-logsDone }()
	// The SDK sends successful results on an unbuffered channel. Always drain
	// its waiter on cancellation too, including an exit racing with timeout.
	waitCtx, cancelWait := context.WithCancel(ctx)
	status, waitErr := cli.ContainerWait(waitCtx, resp.ID, container.WaitConditionNotRunning)
	waitConsumed := false
	defer func() {
		cancelWait()
		if !waitConsumed {
			select {
			case <-status:
			case <-waitErr:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("predeploy canceled: %w", ctx.Err())
	case err := <-waitErr:
		waitConsumed = true
		if err == nil {
			return errors.New("predeploy wait ended without exit status")
		}
		return fmt.Errorf("wait predeploy: %w", err)
	case result := <-status:
		waitConsumed = true
		if result.Error != nil {
			return errors.New("predeploy container failed")
		}
		// Wait for complete output on success, but cancellation still owns the stream.
		select {
		case <-logsDone:
			if logErr != nil {
				return fmt.Errorf("read predeploy logs: %w", logErr)
			}
		case <-ctx.Done():
			return fmt.Errorf("predeploy canceled: %w", ctx.Err())
		}
		if result.StatusCode != 0 {
			return fmt.Errorf("predeploy exited with status %d", result.StatusCode)
		}
	}
	return nil
}

// CleanupPredeploy is called before the worker starts. Labels restrict deletion
// to Krill's interrupted commands; they are never resumed or retried.
func (e *dockerEngine) CleanupPredeploy(ctx context.Context) error {
	nodes, err := e.Nodes(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, node := range nodes {
		nctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cli, release, err := e.verifiedNodeClient(nctx, node.ID)
		if err == nil {
			var list []container.Summary
			list, err = cli.ContainerList(nctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", predeployLabel+"=1"))})
			if err == nil {
				for _, c := range list {
					if e := cli.ContainerRemove(nctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); e != nil && !errdefs.IsNotFound(e) {
						err = errors.Join(err, e)
					}
				}
			}
			release()
		}
		cancel()
		if err != nil {
			result = errors.Join(result, fmt.Errorf("cleanup predeploy node %s: %w", node.ID, err))
		}
	}
	return result
}

var _ PredeployRunner = (*dockerEngine)(nil)

// cancelConn releases its callback on normal close; cancellation closes the raw
// socket directly so it also interrupts req.Write and http.ReadResponse.
type cancelConn struct {
	net.Conn
	stop func() bool
	once sync.Once
}

func (c *cancelConn) Close() error {
	c.once.Do(func() { c.stop() })
	return c.Conn.Close()
}
func predeployAttachClient(ctx context.Context, cli *client.Client) (*client.Client, error) {
	dial := cli.Dialer()
	return client.NewClientWithOpts(client.WithHost(cli.DaemonHost()), client.WithVersion(cli.ClientVersion()),
		client.WithDialContext(func(_ context.Context, _, _ string) (net.Conn, error) {
			raw, err := dial(ctx)
			if err != nil {
				return nil, err
			}
			return &cancelConn{Conn: raw, stop: context.AfterFunc(ctx, func() { raw.Close() })}, nil
		}))
}
