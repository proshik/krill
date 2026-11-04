package docker

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// verifiedNodeClient never treats a missing worker registration as the local
// daemon. Existing exec behavior is deliberately not changed.
func (e *dockerEngine) verifiedNodeClient(ctx context.Context, node string) (*client.Client, func(), error) {
	cli, release, err := selectClient(ctx, e.cli, e.remoteProvider, node)
	if err != nil {
		return nil, func() {}, err
	}
	info, err := cli.Info(ctx)
	if err != nil || info.Swarm.NodeID != node {
		release()
		return nil, func() {}, errors.New("Docker daemon does not match selected node")
	}
	return cli, release, nil
}

// ReleaseValidator checks the dependencies that Docker would otherwise silently
// recreate (volumes), pull by a mutable tag, or schedule on another node.
type ReleaseValidator interface {
	ValidateRelease(context.Context, ServiceSpec, []string) error
}

func (e *dockerEngine) ValidateRelease(ctx context.Context, s ServiceSpec, selected []string) error {
	if !IsImmutableImage(s.Image) {
		return errors.New("immutable release image required")
	}
	nodes, err := e.Nodes(ctx)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, id := range selected {
		wanted[id] = true
	}
	// Placement labels are mutable Docker state, not part of the editor row.
	// Refuse stale or additional label members before validating eligible daemons.
	for _, constraint := range s.Constraints {
		if !strings.HasPrefix(constraint, "node.labels.krill.place.") || !strings.HasSuffix(constraint, "==1") {
			return errors.New("unsupported release placement constraint")
		}
		key := strings.TrimSuffix(strings.TrimPrefix(constraint, "node.labels."), "==1")
		actual, err := e.cli.NodeList(ctx, swarm.NodeListOptions{})
		if err != nil {
			return err
		}
		for _, n := range actual {
			if (n.Spec.Labels[key] == "1") != wanted[n.ID] {
				return errors.New("release placement labels changed")
			}
		}
	}
	restricted := len(selected) > 0
	remaining := len(selected)
	var eligible []SwarmNode
	for _, n := range nodes {
		if restricted && !wanted[n.ID] {
			continue
		}
		if n.State != "ready" || n.Availability != "active" {
			if wanted[n.ID] {
				return errors.New("release placement node is unavailable")
			}
			continue
		}
		eligible = append(eligible, n)
		remaining--
	}
	if (restricted && remaining > 0) || len(eligible) == 0 {
		return errors.New("release placement node no longer exists")
	}
	// Swarm overlays are cluster objects. An idle worker need not have a local
	// copy until a task joins it; only the manager can validate their existence.
	net, err := e.cli.NetworkInspect(ctx, s.Network, network.InspectOptions{})
	if err != nil || net.Driver != "overlay" || net.Scope != "swarm" {
		return errors.New("release organization network unavailable")
	}
	for _, n := range eligible {
		if err := e.validateReleaseOn(ctx, s, n.ID); err != nil {
			return err
		}
	}
	return nil
}
func (e *dockerEngine) validateReleaseOn(ctx context.Context, s ServiceSpec, node string) error {
	cli, release, err := e.verifiedNodeClient(ctx, node)
	if err != nil {
		return err
	}
	defer release()
	for _, m := range s.Mounts {
		if m.Type != "volume" {
			return errors.New("release contains an unsupported mount")
		}
		if _, err := cli.VolumeInspect(ctx, m.Source); err != nil {
			return errors.New("release volume unavailable on placement node")
		}
	}
	if _, _, err := cli.ImageInspectWithRaw(ctx, s.Image); err != nil {
		if !errdefs.IsNotFound(err) || strings.HasPrefix(s.Image, "sha256:") {
			return errors.New("release image unavailable on placement node")
		}
		r, err := cli.ImagePull(ctx, s.Image, image.PullOptions{RegistryAuth: s.RegistryAuth})
		if err != nil {
			return errors.New("release image cannot be pulled by digest")
		}
		_, readErr := io.Copy(io.Discard, r)
		closeErr := r.Close()
		if readErr != nil || closeErr != nil {
			return errors.New("release image pull failed")
		}
		if _, _, err := cli.ImageInspectWithRaw(ctx, s.Image); err != nil {
			return errors.New("release image unavailable after pull")
		}
	}
	return nil
}

var _ ReleaseValidator = (*dockerEngine)(nil)

// IsImmutableImage accepts only a valid content-addressed SHA-256 reference.
func IsImmutableImage(image string) bool {
	ref, err := reference.ParseAnyReference(image)
	if err != nil {
		return false
	}
	d, ok := ref.(reference.Digested)
	return ok && d.Digest().Algorithm() == "sha256" && d.Digest().Validate() == nil
}
