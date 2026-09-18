package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
)

var errObjectNotFound = errors.New("swarm object not found")

// ConfigEnsure creates the config unless one with that name exists. Swarm
// configs are immutable, so callers derive the name from the content.
func (e *dockerEngine) ConfigEnsure(ctx context.Context, name string, data []byte, labels map[string]string) error {
	_, err := e.configID(ctx, name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errObjectNotFound) {
		return err
	}
	_, err = e.cli.ConfigCreate(ctx, swarm.ConfigSpec{Annotations: swarm.Annotations{Name: name, Labels: labels}, Data: data})
	if errdefs.IsConflict(err) {
		return nil // created concurrently under the same name, i.e. with the same content
	}
	return err
}

// SecretEnsure is ConfigEnsure for secrets.
func (e *dockerEngine) SecretEnsure(ctx context.Context, name string, data []byte, labels map[string]string) error {
	_, err := e.secretID(ctx, name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errObjectNotFound) {
		return err
	}
	_, err = e.cli.SecretCreate(ctx, swarm.SecretSpec{Annotations: swarm.Annotations{Name: name, Labels: labels}, Data: data})
	if errdefs.IsConflict(err) {
		return nil
	}
	return err
}

// PruneObjects removes the configs and secrets labelled key=value whose names
// are not in keep. One a service still mounts is skipped: Swarm refuses to
// remove it, and it is freed once the service has moved on.
func (e *dockerEngine) PruneObjects(ctx context.Context, key, value string, keep []string) error {
	keepSet := make(map[string]bool, len(keep))
	for _, n := range keep {
		keepSet[n] = true
	}
	f := filters.NewArgs(filters.Arg("label", key+"="+value))
	var errs []error
	cfgs, err := e.cli.ConfigList(ctx, swarm.ConfigListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, c := range cfgs {
		if keepSet[c.Spec.Name] {
			continue
		}
		if err := e.cli.ConfigRemove(ctx, c.ID); err != nil && !objectInUse(err) && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove config %q: %w", c.Spec.Name, err))
		}
	}
	secs, err := e.cli.SecretList(ctx, swarm.SecretListOptions{Filters: f})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, s := range secs {
		if keepSet[s.Spec.Name] {
			continue
		}
		if err := e.cli.SecretRemove(ctx, s.ID); err != nil && !objectInUse(err) && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove secret %q: %w", s.Spec.Name, err))
		}
	}
	return errors.Join(errs...)
}

// objectInUse recognizes Swarm's refusal to remove a config or secret that a
// service still references. The daemon reports it as a plain InvalidArgument
// with this wording and no dedicated error type.
func objectInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is in use by")
}

func (e *dockerEngine) configID(ctx context.Context, name string) (string, error) {
	list, err := e.cli.ConfigList(ctx, swarm.ConfigListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		return "", err
	}
	for _, c := range list {
		if c.Spec.Name == name { // the name filter matches prefixes
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("swarm config %q: %w", name, errObjectNotFound)
}

func (e *dockerEngine) secretID(ctx context.Context, name string) (string, error) {
	list, err := e.cli.SecretList(ctx, swarm.SecretListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if s.Spec.Name == name {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("swarm secret %q: %w", name, errObjectNotFound)
}

// resolveRefs returns a copy of refs with each object's ID filled in.
func resolveRefs(ctx context.Context, refs []FileRef, lookup func(context.Context, string) (string, error)) ([]FileRef, error) {
	if len(refs) == 0 {
		return refs, nil
	}
	out := make([]FileRef, len(refs))
	for i, r := range refs {
		id, err := lookup(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		r.ID = id
		out[i] = r
	}
	return out, nil
}
