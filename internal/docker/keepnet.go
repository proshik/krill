package docker

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
)

// attachmentNames resolves each attachment's target — Swarm stores a network
// ID even when the spec named it — to the network's name. A network that is
// gone is left out: there is nothing to keep.
func (e *dockerEngine) attachmentNames(ctx context.Context, atts []swarm.NetworkAttachmentConfig) (map[string]string, error) {
	out := make(map[string]string, len(atts))
	for _, a := range atts {
		n, err := e.cli.NetworkInspect(ctx, a.Target, network.InspectOptions{})
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		out[a.Target] = n.Name
	}
	return out, nil
}

// keepAttachments returns want plus every attachment of cur whose network
// name starts with prefix, aliases and driver options as they were, unless
// want already attaches that network. names maps cur's targets to names.
func keepAttachments(cur []swarm.NetworkAttachmentConfig, names map[string]string, want []swarm.NetworkAttachmentConfig, prefix string) []swarm.NetworkAttachmentConfig {
	have := make(map[string]bool, len(want))
	for _, w := range want {
		have[w.Target] = true
	}
	out := want
	for _, c := range cur {
		name := names[c.Target]
		if name == "" || !strings.HasPrefix(name, prefix) || have[name] || have[c.Target] {
			continue
		}
		kept := c
		kept.Aliases = append([]string(nil), c.Aliases...)
		out = append(out, kept)
		have[name] = true
	}
	return out
}
