package link

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/proshik/krill/internal/netguard"
)

// Limits of one egress stream on the relay.
const (
	// egressHelloTimeout bounds how long the client may take to send the
	// app's ClientHello after the stream header.
	egressHelloTimeout = 10 * time.Second
	// egressDialTimeout bounds connecting to the destination.
	egressDialTimeout = 10 * time.Second
)

// Bounds of a manifest the relay keeps: it is shown, never trusted.
const (
	maxManifestServices = 256
	maxManifestDests    = 64
	maxManifestPath     = 200
)

// serveEgress serves one egress stream of a session: the destination must be
// on the relay's list, the ClientHello inside must name exactly that host, and
// the relay resolves and dials the host itself, refusing private addresses.
// TLS stays end to end: the relay sees the host, the port and the sizes, never
// the content.
func (r *Relay) serveEgress(st *yamux.Stream, body []byte, s *relaySession) {
	var h egressHeader
	if err := json.Unmarshal(body, &h); err != nil {
		slog.Warn("link: egress refused, malformed header", "peer", s.peer)
		return
	}
	d, err := ParseDestination(h.Host, strconv.Itoa(h.Port))
	if err != nil || d.Host != h.Host {
		slog.Warn("link: egress refused, not an exact host and port", "peer", s.peer, "host_len", len(h.Host))
		return
	}
	dest := d.String()
	cell := r.traffic.cell(trafficKey{Peer: s.peerID, Service: h.Service, Dest: dest})
	allowed, err := r.store.destinationAllowed(r.ctx, d)
	if err != nil {
		cell.refused.Add(1)
		slog.Error("link: egress: read the destination list failed", "peer", s.peer, "err", err)
		return
	}
	if !allowed {
		cell.refused.Add(1)
		slog.Warn("link: egress refused, destination not on the relay's list", "peer", s.peer, "service", h.Service, "dest", dest)
		return
	}
	// The relay's own check of the name the app asks for: without it a
	// compromised client could reach another virtual host behind an allowed
	// name's shared TLS front end.
	_ = st.SetReadDeadline(time.Now().Add(egressHelloTimeout))
	hello, sni, err := ReadClientHello(st)
	if err != nil {
		cell.refused.Add(1)
		slog.Warn("link: egress refused, no TLS ClientHello with a server name", "peer", s.peer, "service", h.Service, "dest", dest)
		return
	}
	if sni != d.Host {
		cell.refused.Add(1)
		slog.Warn("link: egress refused, the ClientHello names another host", "peer", s.peer, "service", h.Service, "dest", dest)
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	dctx, cancel := context.WithTimeout(r.ctx, egressDialTimeout)
	up, err := r.dial(dctx, "tcp", dest)
	cancel()
	if err != nil {
		cell.refused.Add(1)
		slog.Warn("link: egress: destination unreachable", "peer", s.peer, "service", h.Service, "dest", dest, "category", dialCategory(err))
		return
	}
	defer up.Close()
	if _, err := up.Write(hello); err != nil {
		cell.refused.Add(1)
		return
	}
	cell.up.Add(int64(len(hello)))
	cell.conns.Add(1)
	splice(st, up, &cell.up, &cell.down, s.mux.CloseChan())
}

// dialCategory classifies a failed dial for the log, without the resolver's
// or the peer's own words.
func dialCategory(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		return "blocked"
	case errors.As(err, &dns):
		return CatDNS
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return CatTCP
}

// boundManifest keeps a manifest to what the relay shows.
func boundManifest(in []ManifestService) []ManifestService {
	if len(in) > maxManifestServices {
		in = in[:maxManifestServices]
	}
	out := make([]ManifestService, 0, len(in))
	for _, m := range in {
		if len(m.Path) > maxManifestPath {
			m.Path = m.Path[:maxManifestPath]
		}
		if len(m.Destinations) > maxManifestDests {
			m.Destinations = m.Destinations[:maxManifestDests]
		}
		ds := make([]string, 0, len(m.Destinations))
		for _, d := range m.Destinations {
			if p, err := ParseDestinationString(d); err == nil {
				ds = append(ds, p.String())
			}
		}
		m.Destinations = ds
		out = append(out, m)
	}
	return out
}

// pushLists sends the destination list to every session whose client speaks
// egress and has had its hello. The list is read under the session's write
// lock, so of two pushes the later one sends the newer list, and a session
// still before its hello reads the list itself when it sends the hello.
func (r *Relay) pushLists(ctx context.Context) {
	for _, sc := range r.reg.all() {
		s, ok := sc.(*relaySession)
		if !ok || !s.egress {
			continue
		}
		if err := s.sendLists(ctx, r.store); err != nil {
			slog.Info("link: lists not delivered; the session ends", "peer", s.peer, "err", err)
			_ = s.mux.Close()
		}
	}
}

// sendLists writes the current list to a session that has had its hello.
func (s *relaySession) sendLists(ctx context.Context, store *Store) error {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	if !s.ready {
		return nil
	}
	dests, err := store.Destinations(ctx)
	if err != nil {
		return err
	}
	return writeMessage(s.ctl, message{Type: "lists", Destinations: dests})
}

// sendHello answers the client's hello: first on the control stream, with
// the destination list for a client that speaks egress, read under the
// session's write lock so no lists can precede or outdate it.
func (s *relaySession) sendHello(ctx context.Context, store *Store, version string) error {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	m := message{Type: "hello", Proto: ProtoVersion, KrillVersion: version, Egress: true}
	if s.egress {
		dests, err := store.Destinations(ctx)
		if err != nil {
			// The client then refuses every egress connection until the
			// next lists; the session itself is fine.
			slog.Error("link: read the destination list failed", "peer", s.peer, "err", err)
		}
		m.Destinations = dests
	}
	if err := writeMessage(s.ctl, m); err != nil {
		return err
	}
	s.ready = true
	return nil
}
