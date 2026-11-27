package hostshare

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
)

// Reservation names the installation-level record that holds a host outside
// the domains table. Such a host belongs to Krill itself: an application's
// router there would compete with the panel's login form, and a domain on the
// link host would be swallowed by the link's TCP router, which Traefik tries
// before every HTTP router.
type Reservation string

const (
	NotReserved       Reservation = ""
	ReservedByPanel   Reservation = "panel"   // the panel domain (panel_gateway)
	ReservedByLink    Reservation = "link"    // the relay's link host (link_settings)
	ReservedByIngress Reservation = "ingress" // a relay ingress host (link_ingress_hosts)
)

// i18n keys of a reservation conflict, rendered by the server.
const (
	KeyReservedPanel = "flash.err.host_reserved_panel"
	KeyReservedLink  = "flash.err.host_reserved_link"
)

// Key is the i18n key explaining the reservation, "" for NotReserved.
func (r Reservation) Key() string {
	switch r {
	case ReservedByPanel:
		return KeyReservedPanel
	case ReservedByLink, ReservedByIngress:
		return KeyReservedLink
	}
	return ""
}

// Reserved reports which installation record holds host. It takes no lock: a
// handler uses it to explain a refusal up front, while every write checks it
// again under the host lock (reservedErr).
func Reserved(ctx context.Context, q *db.Queries, host string) (Reservation, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	panel, err := q.GetPanelGateway(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return NotReserved, fmt.Errorf("read panel settings: %w", err)
	case panel.State != "off" && strings.EqualFold(panel.Host, host):
		return ReservedByPanel, nil
	}
	if r, err := linkReservation(ctx, q, host); err != nil || r != NotReserved {
		return r, err
	}
	return NotReserved, nil
}

// linkReservation is the part of Reserved the link owns: the relay's link host
// and its ingress hosts.
func linkReservation(ctx context.Context, q *db.Queries, host string) (Reservation, error) {
	linkHost, err := q.GetLinkRelayHost(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return NotReserved, fmt.Errorf("read link settings: %w", err)
	case linkHost != "" && strings.EqualFold(linkHost, host):
		return ReservedByLink, nil
	}
	n, err := q.CountLinkIngressHostsByHost(ctx, host)
	if err != nil {
		return NotReserved, fmt.Errorf("read link ingress hosts: %w", err)
	}
	if n > 0 {
		return ReservedByIngress, nil
	}
	return NotReserved, nil
}

// reservedErr is the reserved-host check every domain write makes under the
// host lock shared with SetPanelDomain and ReserveLinkHost.
func reservedErr(ctx context.Context, q *db.Queries, host string) error {
	r, err := Reserved(ctx, q, host)
	if err != nil {
		return err
	}
	if r != NotReserved {
		return &ConflictError{Key: r.Key()}
	}
	return nil
}

// ReserveLinkHost runs write — the caller's update of the link settings —
// under host's lock, after checking that neither a domain row nor the panel
// domain holds host. A link host that a domain already claims would take that
// domain's HTTPS away (TCP routers are tried first), so it is refused rather
// than routed.
func (s *Store) ReserveLinkHost(ctx context.Context, host string, write func(q *db.Queries) error) error {
	return s.inHostTx(ctx, host, func(q *db.Queries) error {
		panel, err := q.GetPanelGateway(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read panel settings: %w", err)
		case panel.State != "off" && strings.EqualFold(panel.Host, host):
			return &ConflictError{Key: KeyReservedPanel}
		}
		n, err := q.CountDomainsByHost(ctx, host)
		if err != nil {
			return err
		}
		if n > 0 {
			return &ConflictError{Key: KeyHostTaken}
		}
		return write(q)
	})
}

// HostHolder is a record that holds a host the link wants: the panel domain or
// an application's domain row.
type HostHolder struct {
	Panel     bool
	OrgID     int64
	ProjectID int64
	EnvID     int64
	AppID     int64
	AppName   string
	Project   string
	Env       string
}

// LinkHostHolders re-checks, under host's lock, whether the panel domain or a
// domain row holds the link host. A release without the link never knew the
// host was taken, so after a rollback a domain can sit on it; the link's route
// must then not be published until the operator removes one of them.
func (s *Store) LinkHostHolders(ctx context.Context, host string) ([]HostHolder, error) {
	var out []HostHolder
	err := s.inHostTx(ctx, host, func(q *db.Queries) error {
		panel, err := q.GetPanelGateway(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read panel settings: %w", err)
		case panel.State != "off" && strings.EqualFold(panel.Host, host):
			out = append(out, HostHolder{Panel: true})
		}
		rows, err := q.ListLinkHostDomainHolders(ctx, host)
		if err != nil {
			return fmt.Errorf("list domain holders: %w", err)
		}
		for _, r := range rows {
			out = append(out, HostHolder{
				OrgID: r.OrgID, ProjectID: r.ProjectID, EnvID: r.EnvironmentID, AppID: r.AppID,
				AppName: r.AppName, Project: r.ProjectName, Env: r.EnvName,
			})
		}
		return nil
	})
	return out, err
}
