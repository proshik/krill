package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/proshik/krill/internal/link"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// rollbackProber is the part of *selfupdate.Updater the egress rollback gate
// needs.
type rollbackProber interface {
	RollbackBinary(ctx context.Context) selfupdate.RollbackBinary
}

var _ rollbackProber = (*selfupdate.Updater)(nil)

// rollbackGateTimeout bounds reading the rollback binary's version (it is
// cached per file once read).
const rollbackGateTimeout = 15 * time.Second

// egressGate checks the rollback binary before egress is enabled: nil when
// there is none or it is at least the link core release, a
// *link.RollbackRefusal otherwise — also when the updater is not wired and
// the binary cannot be told (fail closed).
func (s *Server) egressGate(ctx context.Context) error {
	p, ok := s.updates.installer.(rollbackProber)
	if !ok {
		return &link.RollbackRefusal{}
	}
	ctx, cancel := context.WithTimeout(ctx, rollbackGateTimeout)
	defer cancel()
	b := p.RollbackBinary(ctx)
	return link.RollbackGate(b.Kind, b.Version)
}

// gateText is the reason the gate gives, translated.
func gateText(ctx context.Context, err error) string {
	var r *link.RollbackRefusal
	if !errors.As(err, &r) {
		return ""
	}
	if r.Prev == "" {
		return i18n.Tf(ctx, "link.gate.unknown", link.CoreRelease)
	}
	return i18n.Tf(ctx, "link.gate.older", r.Prev, link.CoreRelease)
}

// gateView is the gate as the pages show it.
func (s *Server) gateView(ctx context.Context, orgID int64) templates.LinkGate {
	err := s.egressGate(ctx)
	if err == nil {
		return templates.LinkGate{}
	}
	return templates.LinkGate{Closed: true, Text: gateText(ctx, err), UpdatesURL: orgURL(orgID) + "/updates"}
}

func orgURL(orgID int64) string { return "/orgs/" + strconv.FormatInt(orgID, 10) }

// egressConfig serves the egress sidecar's dynamic configuration to its HTTP
// provider. Anything but a poll with the sidecar's token gets a 404, and so
// does a request the gateway proxied. A read error answers 500, never an
// empty configuration: the provider keeps its routes on a failed poll.
func (s *Server) egressConfig(w http.ResponseWriter, r *http.Request) {
	var e *link.Egress
	if s.link != nil {
		e = s.link.Egress()
	}
	if e == nil || !panel.Matches(r.Header.Get(link.EgressHeader), e.Token()) || s.viaGateway(r) {
		http.NotFound(w, r)
		return
	}
	cfg, err := e.DynamicConfig(r.Context())
	if err != nil {
		logFrom(r).Error("egressConfig: build the sidecar's routes failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		logFrom(r).Warn("egressConfig: write failed", "err", err)
	}
}

// fillLinkEgress adds the egress parts to the Link page.
func (s *Server) fillLinkEgress(r *http.Request, orgID int64, role string, v *templates.LinkView) error {
	ctx := r.Context()
	switch role {
	case link.RoleRelay:
		ds, err := s.link.Destinations(ctx)
		if err != nil {
			return err
		}
		v.Dests = destRows(ds)
		for i := range v.Peers {
			svcs, err := s.link.PeerServices(ctx, v.Peers[i].ID)
			if err != nil {
				return err
			}
			v.Peers[i].Services = serviceRows(svcs)
		}
	case link.RoleClient:
		ds, ok := s.link.ClientRelayDestinations()
		v.Dests, v.RelayEgress = destRows(ds), ok
		grants, err := s.link.Grants(ctx)
		if err != nil {
			return err
		}
		orgs, err := s.q.ListOrganizations(ctx)
		if err != nil {
			return err
		}
		for _, o := range orgs {
			v.Grants = append(v.Grants, templates.LinkGrantRow{OrgID: o.ID, Name: o.Name, Granted: grants[o.ID]})
		}
		svcs, err := s.link.ClientServices(ctx)
		if err != nil {
			return err
		}
		v.Services = serviceRows(svcs)
		v.Gate = s.gateView(ctx, orgID)
		if e := s.link.Egress(); e == nil {
			v.EgressReqKey = "link.egress_unwired"
		} else {
			v.EgressReqKey = relayRequirementKey(e.Requirement())
		}
	}
	return nil
}

func destRows(ds []link.Destination) []templates.LinkDestRow {
	out := make([]templates.LinkDestRow, 0, len(ds))
	for _, d := range ds {
		out = append(out, templates.LinkDestRow{Host: d.Host, Port: d.Port})
	}
	return out
}

func serviceRows(svcs []link.ServiceView) []templates.LinkServiceRow {
	out := make([]templates.LinkServiceRow, 0, len(svcs))
	for _, sv := range svcs {
		row := templates.LinkServiceRow{Path: sv.Path}
		for _, d := range sv.Dests {
			row.Dests = append(row.Dests, templates.LinkServiceDest{Dest: d.Dest, Allowed: d.Allowed, Up: d.Up, Down: d.Down})
		}
		out = append(out, row)
	}
	return out
}

// destinationFromForm reads host and port from a form.
func destinationFromForm(r *http.Request) (link.Destination, error) {
	return link.ParseDestination(r.FormValue("host"), r.FormValue("port"))
}

// addLinkDestination adds a destination to the relay's list.
func (s *Server) addLinkDestination(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	d, err := destinationFromForm(r)
	if err != nil {
		s.flashErrT(w, r, "link.err.invalid_dest")
		return
	}
	switch err := s.link.AddDestination(r.Context(), d); {
	case errors.Is(err, link.ErrWrongRole):
		s.flashErrT(w, r, "link.err.not_relay_dests")
		return
	case err != nil:
		logFrom(r).Error("addLinkDestination: add failed", "dest", d.String(), "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link destination added", "dest", d.String())
	s.flashOK(w, r, "link.ok.dest_added")
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}

// removeLinkDestination removes a destination from the relay's list.
func (s *Server) removeLinkDestination(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	d, err := destinationFromForm(r)
	if err != nil {
		s.flashErrT(w, r, "link.err.invalid_dest")
		return
	}
	switch err := s.link.RemoveDestination(r.Context(), d); {
	case errors.Is(err, link.ErrWrongRole):
		s.flashErrT(w, r, "link.err.not_relay_dests")
		return
	case err != nil:
		logFrom(r).Error("removeLinkDestination: remove failed", "dest", d.String(), "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link destination removed", "dest", d.String())
	s.flashOK(w, r, "link.ok.dest_removed")
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}

// setLinkGrant grants (enabled=1) or revokes (enabled=0) an organization. A
// grant enables egress and passes the rollback gate first; a revoke never
// waits for it.
func (s *Server) setLinkGrant(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	target, err := strconv.ParseInt(r.FormValue("org_id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	on := r.FormValue("enabled") == "1"
	if !on && r.FormValue("enabled") != "0" {
		http.Error(w, "enabled must be 1 or 0", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if _, err := s.q.GetOrganization(ctx, target); err != nil {
		http.NotFound(w, r)
		return
	}
	if on {
		if err := s.egressGate(ctx); err != nil {
			logFrom(r).Warn("setLinkGrant: refused by the rollback gate", "org_id", target, "err", err)
			s.flashErr(w, r, gateText(ctx, err))
			return
		}
	}
	switch err := s.link.SetGrant(ctx, target, on); {
	case errors.Is(err, link.ErrWrongRole):
		s.flashErrT(w, r, "link.err.not_client")
		return
	case err != nil:
		logFrom(r).Error("setLinkGrant: change failed", "org_id", target, "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link organization grant set", "org_id", target, "granted", on)
	if on {
		s.flashOK(w, r, "link.ok.granted")
	} else {
		s.flashOK(w, r, "link.ok.revoked")
	}
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}

// appLinkView is the app's egress card, nil when the link is not open to the
// app's organization (no card, and the POST is a 404).
func (s *Server) appLinkView(ctx context.Context, orgID, appID int64) (*templates.AppLinkView, error) {
	if s.link == nil {
		return nil, nil
	}
	granted, err := s.link.Granted(ctx, orgID)
	if err != nil || !granted {
		return nil, err
	}
	saved, err := s.link.AppDestinations(ctx, appID)
	if err != nil {
		return nil, err
	}
	offered, _ := s.link.ClientRelayDestinations()
	v := &templates.AppLinkView{Connected: s.link.ClientStatus().State == link.StateConnected, Gate: s.gateView(ctx, orgID)}
	seen := map[link.Destination]bool{}
	for _, d := range offered {
		seen[d] = true
		v.Dests = append(v.Dests, templates.AppLinkDest{Dest: d.String(), Allowed: true, Checked: contains(saved, d)})
	}
	for _, d := range saved {
		if !seen[d] {
			v.Dests = append(v.Dests, templates.AppLinkDest{Dest: d.String(), Checked: true})
		}
	}
	return v, nil
}

func contains(ds []link.Destination, d link.Destination) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

// saveAppLink saves the app's egress selection. Without the link open to the
// app's organization the route does not exist (404); with the rollback gate
// closed nothing is written.
func (s *Server) saveAppLink(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	granted, err := s.link.Granted(ctx, c.Org.ID)
	if err != nil {
		logFrom(r).Error("saveAppLink: read the grant failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if !granted {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var ds []link.Destination
	for _, raw := range r.Form["dest"] {
		d, err := link.ParseDestinationString(raw)
		if err != nil {
			http.Error(w, "invalid destination", http.StatusBadRequest)
			return
		}
		ds = append(ds, d)
	}
	if err := s.egressGate(ctx); err != nil {
		logFrom(r).Warn("saveAppLink: refused by the rollback gate", "err", err)
		s.flashErr(w, r, gateText(ctx, err))
		return
	}
	switch err := s.link.SetAppDestinations(ctx, c.Org.ID, c.App.ID, ds); {
	case errors.Is(err, link.ErrNotGranted):
		http.NotFound(w, r)
		return
	case errors.Is(err, link.ErrNotOffered):
		s.flashErrT(w, r, "applink.err.not_offered")
		return
	case err != nil:
		logFrom(r).Error("saveAppLink: save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("app link destinations saved", "destinations", len(ds))
	s.flashOK(w, r, "applink.ok")
	http.Redirect(w, r, appURL(c)+"?tab=settings#s-link", http.StatusSeeOther)
}
