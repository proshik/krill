package link

import (
	"context"
	"errors"
	"log/slog"
	"slices"
)

// ErrNotGranted refuses an app's selection in an organization the link is
// not granted to, or while the installation is not a client.
var ErrNotGranted = errors.New("the organization is not granted the link")

// ErrNotOffered refuses a selected destination the relay did not send.
var ErrNotOffered = errors.New("the relay does not offer this destination")

// Egress is the client's egress, nil when it is not wired.
func (s *Service) Egress() *Egress { return s.egressOf() }

// Destinations is the relay's destination list (relay).
func (s *Service) Destinations(ctx context.Context) ([]Destination, error) {
	return s.store.Destinations(ctx)
}

// AddDestination adds d to the relay's list and sends the new list to the
// clients.
func (s *Service) AddDestination(ctx context.Context, d Destination) error {
	if s.Role() != RoleRelay {
		return ErrWrongRole
	}
	added, err := s.store.addDestination(ctx, d)
	if err != nil {
		return err
	}
	if added {
		slog.Info("link: destination added", "dest", d.String())
		go s.relay.pushLists(context.WithoutCancel(ctx))
	}
	return nil
}

// RemoveDestination removes d from the relay's list: new egress streams for
// it are refused at once, and the clients get the new list.
func (s *Service) RemoveDestination(ctx context.Context, d Destination) error {
	if s.Role() != RoleRelay {
		return ErrWrongRole
	}
	removed, err := s.store.removeDestination(ctx, d)
	if err != nil {
		return err
	}
	if removed {
		slog.Info("link: destination removed", "dest", d.String())
		go s.relay.pushLists(context.WithoutCancel(ctx))
	}
	return nil
}

// Grants lists the organizations granted the link (client).
func (s *Service) Grants(ctx context.Context) (map[int64]bool, error) { return s.store.Grants(ctx) }

// Granted reports whether the link is open to an organization's apps: the
// installation is a client and the organization is granted.
func (s *Service) Granted(ctx context.Context, orgID int64) (bool, error) {
	if s.Role() != RoleClient {
		return false, nil
	}
	return s.store.Granted(ctx, orgID)
}

// SetGrant grants or revokes an organization. A revoke closes the apps' new
// connections at once: every connection reads the grant.
func (s *Service) SetGrant(ctx context.Context, orgID int64, on bool) error {
	if s.Role() != RoleClient {
		return ErrWrongRole
	}
	if err := s.store.setGrant(ctx, orgID, on); err != nil {
		return err
	}
	slog.Info("link: organization grant changed", "org_id", orgID, "granted", on)
	s.egressChanged(ctx)
	return nil
}

// AppDestinations is an app's saved selection.
func (s *Service) AppDestinations(ctx context.Context, appID int64) ([]Destination, error) {
	return s.store.AppDestinations(ctx, appID)
}

// SetAppDestinations replaces an app's saved selection. Every destination
// must be one the relay sent, except those already saved (the relay may have
// dropped one since: unselecting it must stay possible, keeping it changes
// nothing). The organization must be granted.
func (s *Service) SetAppDestinations(ctx context.Context, orgID, appID int64, ds []Destination) error {
	ok, err := s.Granted(ctx, orgID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotGranted
	}
	saved, err := s.store.AppDestinations(ctx, appID)
	if err != nil {
		return err
	}
	offered, _ := s.client.RelayDestinations()
	ds = SortDestinations(ds)
	for _, d := range ds {
		if !slices.Contains(offered, d) && !slices.Contains(saved, d) {
			return ErrNotOffered
		}
	}
	if err := s.store.setAppDestinations(ctx, appID, ds); err != nil {
		return err
	}
	slog.Info("link: app destinations saved", "app", appID, "destinations", len(ds))
	s.egressChanged(ctx)
	return nil
}

// egressChanged lets egress follow a change: the sidecar's names and routes,
// and the manifest.
func (s *Service) egressChanged(ctx context.Context) {
	if e := s.egressOf(); e != nil {
		e.Kick()
		e.publishManifest(ctx)
	}
}

// ServiceView is one client service on a Link page: its destinations with
// their state and counters.
type ServiceView struct {
	ID    int64
	Path  string
	Dests []DestView
}

// DestView is one destination of a service.
type DestView struct {
	Dest    string
	Allowed bool // on the relay's list (client) / on this relay's list (relay)
	Up      int64
	Down    int64
	Conns   int64
	Refused int64
}

// ClientServices lists the client's services: every app of a granted
// organization with a saved selection, with the relay's word on each
// destination and the counters since this process started.
func (s *Service) ClientServices(ctx context.Context) ([]ServiceView, error) {
	apps, err := s.store.egressApps(ctx)
	if err != nil {
		return nil, err
	}
	offered, _ := s.client.RelayDestinations()
	var counters []Traffic
	if e := s.egressOf(); e != nil {
		counters = e.traffic.of(0)
	}
	var out []ServiceView
	for _, m := range manifestOf(apps) {
		v := ServiceView{ID: m.ID, Path: m.Path}
		for _, ds := range m.Destinations {
			d, _ := ParseDestinationString(ds)
			dv := DestView{Dest: ds, Allowed: slices.Contains(offered, d)}
			addCounters(&dv, counters, m.ID)
			v.Dests = append(v.Dests, dv)
		}
		out = append(out, v)
	}
	return out, nil
}

// PeerServices lists a relay peer's services from its manifest, with this
// relay's word on each destination and its counters.
func (s *Service) PeerServices(ctx context.Context, peerID int64) ([]ServiceView, error) {
	sess, ok := s.relay.Session(peerID)
	if !ok {
		return nil, nil
	}
	list, err := s.store.Destinations(ctx)
	if err != nil {
		return nil, err
	}
	counters := s.relay.PeerTraffic(peerID)
	var out []ServiceView
	for _, m := range sess.Services {
		v := ServiceView{ID: m.ID, Path: m.Path}
		for _, ds := range m.Destinations {
			d, _ := ParseDestinationString(ds)
			dv := DestView{Dest: ds, Allowed: slices.Contains(list, d)}
			addCounters(&dv, counters, m.ID)
			v.Dests = append(v.Dests, dv)
		}
		out = append(out, v)
	}
	return out, nil
}

func addCounters(dv *DestView, counters []Traffic, service int64) {
	for _, c := range counters {
		if c.Service == service && c.Dest == dv.Dest {
			dv.Up, dv.Down, dv.Conns, dv.Refused = c.Up, c.Down, c.Conns, c.Refused
		}
	}
}
