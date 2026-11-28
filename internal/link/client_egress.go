package link

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"time"
)

// RelayDestinations is the relay's destination list as it last said, and
// whether the relay speaks egress at all (false before the first session or
// with a relay without egress).
func (c *Client) RelayDestinations() ([]Destination, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.dests), c.relayEgress
}

// relayAllows reports whether d is on the relay's list of the live session.
func (c *Client) relayAllows(d Destination) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur != nil && c.relayEgress && slices.Contains(c.dests, d)
}

func (c *Client) setDests(ds []Destination) {
	c.mu.Lock()
	c.dests = SortDestinations(ds)
	c.mu.Unlock()
	slog.Info("link: the relay's destination list changed", "destinations", len(ds))
}

// sortManifest orders services by id.
func sortManifest(m []ManifestService) {
	slices.SortFunc(m, func(a, b ManifestService) int { return int(a.ID - b.ID) })
}

// SetManifest records the client's services and sends them to the relay when
// a session that speaks egress is up; every new session sends the latest.
func (c *Client) SetManifest(svcs []ManifestService) {
	c.mu.Lock()
	c.manifest = slices.Clone(svcs)
	c.mu.Unlock()
	c.sendManifest()
}

// sendManifest writes the manifest on the live control stream, bounded to
// what one control message carries.
func (c *Client) sendManifest() {
	// The snapshot is taken under the write lock: of two sends the later
	// one carries the newer manifest.
	c.ctlMu.Lock()
	defer c.ctlMu.Unlock()
	c.mu.Lock()
	ctl, ok, svcs := c.ctl, c.relayEgress, c.manifest
	c.mu.Unlock()
	if ctl == nil || !ok {
		return
	}
	m := message{Type: "manifest", Services: fitManifest(svcs)}
	err := writeMessage(ctl, m)
	if err != nil {
		slog.Info("link: manifest not delivered", "err", err)
	}
}

// fitManifest drops services from the end until the message fits a control
// line: the manifest is for display, the relay's list is the boundary.
func fitManifest(svcs []ManifestService) []ManifestService {
	out := boundManifest(svcs)
	for len(out) > 0 {
		b, err := jsonLen(message{Type: "manifest", Services: out})
		if err == nil && b < maxMessage-1 {
			break
		}
		out = out[:len(out)-1]
	}
	return out
}

// OpenEgress opens an egress stream for d on the live session, labelled with
// service, and writes its header; the channel closes when that session ends.
// ErrEgressNoSession when no session is up or the relay does not speak
// egress.
func (c *Client) OpenEgress(ctx context.Context, d Destination, service int64) (net.Conn, <-chan struct{}, error) {
	c.mu.Lock()
	mux, ok := c.cur, c.relayEgress
	c.mu.Unlock()
	if mux == nil || !ok {
		return nil, nil, ErrEgressNoSession
	}
	st, err := mux.OpenStream()
	if err != nil {
		return nil, nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = st.SetWriteDeadline(dl)
	} else {
		_ = st.SetWriteDeadline(time.Now().Add(HeaderTimeout))
	}
	if err := WriteHeader(st, StreamEgress, egressHeader{Host: d.Host, Port: d.Port, Service: service}); err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	_ = st.SetWriteDeadline(time.Time{})
	return st, mux.CloseChan(), nil
}
