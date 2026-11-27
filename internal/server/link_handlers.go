package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/link"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/web/templates"
)

// flashLinkCodeCookie carries a fresh pairing code to the page that shows it,
// once (as flashTokenCookie does for API tokens). The code is a one-time
// credential: it is never stored in plain form nor logged.
const flashLinkCodeCookie = "krill_flash_link_code"

// SetLink wires the Krill link; nil leaves the Link page unavailable.
func (s *Server) SetLink(l *link.Service) { s.link = l }

func linkBack(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/link"
}

func (s *Server) setFlashLinkCode(w http.ResponseWriter, r *http.Request, code string) {
	http.SetCookie(w, &http.Cookie{
		Name: flashLinkCodeCookie, Value: code, Path: "/", HttpOnly: true,
		Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: 120,
	})
}

func (s *Server) takeFlashLinkCode(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashLinkCodeCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashLinkCodeCookie, Path: "/", MaxAge: -1, HttpOnly: true})
	return c.Value
}

// relayRequirementKey is the i18n key explaining why the relay role is not
// available, "" when it is.
func relayRequirementKey(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, panel.ErrAdvertiseUnset):
		return "link.req.advertise"
	case errors.Is(err, panel.ErrListenLoopback):
		return "link.req.loopback"
	case errors.Is(err, link.ErrLinkListenLoopback):
		return "link.req.link_loopback"
	case errors.Is(err, link.ErrLinkPortTaken):
		return "link.req.port_taken"
	}
	return "link.req.listen"
}

// linkPage renders Settings → Link. Instance-admin only.
func (s *Server) linkPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	v := templates.LinkView{Available: s.link != nil}
	if s.link == nil {
		render(w, r, http.StatusOK, templates.Link(o, role, v))
		return
	}
	ctx := r.Context()
	row, err := s.link.Settings(ctx)
	if err != nil {
		logFrom(r).Error("linkPage: read link settings failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v.Role = row.Role
	v.ListenAddr = s.cfg.LinkListenAddr
	v.RelayReqKey = relayRequirementKey(s.link.RelayRequirement())
	v.ClientReqKey = relayRequirementKey(s.link.ListenRequirement())
	if err := s.link.ListenError(); err != nil {
		v.ListenFailed = true
	}
	v.RelayCertsBroken = s.link.RelayError() != nil
	v.FirewallURL = "/orgs/" + strconv.FormatInt(o.ID, 10) + "/firewall"
	if row.Role != link.RoleOff {
		v.FirewallStale = s.linkFirewallStale(r)
	}
	switch row.Role {
	case link.RoleRelay:
		if row.LinkHost != nil {
			v.LinkHost = *row.LinkHost
		}
		v.DefaultAddr = v.LinkHost + ":443"
		v.NewCode = s.takeFlashLinkCode(w, r)
		holders, err := s.link.LinkHostConflicts(ctx)
		if err != nil {
			logFrom(r).Error("linkPage: re-check link host failed", "err", err)
		}
		for _, h := range holders {
			c := templates.LinkConflict{Panel: h.Panel, Ingress: h.Ingress}
			if !h.Panel && !h.Ingress {
				c.App = h.Project + "/" + h.Env + "/" + h.AppName
				c.URL = "/orgs/" + strconv.FormatInt(h.OrgID, 10) + "/projects/" + strconv.FormatInt(h.ProjectID, 10) +
					"/environments/" + strconv.FormatInt(h.EnvID, 10) + "/apps/" + strconv.FormatInt(h.AppID, 10) + "/domains"
			}
			v.Conflicts = append(v.Conflicts, c)
		}
		peers, err := s.link.Peers(ctx)
		if err != nil {
			logFrom(r).Error("linkPage: list peers failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, p := range peers {
			pr := templates.LinkPeerRow{ID: p.Peer.ID, Name: p.Peer.Name, CertNotAfter: p.Peer.CertNotAfter, Connected: p.Connected}
			if p.Connected {
				pr.From = p.Session.From.String()
				pr.Since = p.Session.Since
				pr.Version = p.Session.Version
				pr.Latency = p.Session.Latency
			}
			if p.Peer.LastDisconnectedAt.Valid {
				pr.LastSeen = p.Peer.LastDisconnectedAt.Time
			}
			if p.Refusal != nil {
				pr.RefusalCategory = p.Refusal.Category
				pr.RefusalProto = p.Refusal.Proto
				pr.RefusalFrom = p.Refusal.From.String()
				pr.RefusalAt = p.Refusal.At
			}
			v.Peers = append(v.Peers, pr)
		}
		codes, err := s.link.OpenCodes(ctx)
		if err != nil {
			logFrom(r).Error("linkPage: list pairing codes failed", "err", err)
		}
		for _, c := range codes {
			v.OpenCodes = append(v.OpenCodes, templates.LinkCodeRow{Peer: c.PeerName, ExpiresAt: c.ExpiresAt})
		}
	case link.RoleClient:
		st := s.link.ClientStatus()
		v.Client = templates.LinkClientState{
			State: st.State, Category: st.Category, Message: st.Message, FailedAt: st.FailedAt,
			Since: st.Since, Latency: st.Latency, RelayVersion: st.RelayVersion, CertNotAfter: st.CertNotAfter,
		}
		if row.RelaySni != nil {
			v.RelaySNI = *row.RelaySni
		}
		if row.RelayAddr != nil {
			v.RelayAddr = *row.RelayAddr
		}
		v.Paired = v.RelayAddr != ""
		if row.PairedAt.Valid {
			v.PairedAt = row.PairedAt.Time
		}
	}
	if err := s.fillLinkEgress(r, o.ID, row.Role, &v); err != nil {
		logFrom(r).Error("linkPage: read egress failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	render(w, r, http.StatusOK, templates.Link(o, role, v))
}

// linkFirewallStale reports whether the control plane is locked down with a
// ruleset other than the one it needs now — under a lockdown applied before
// the link role was turned on, the link listener's port stays closed until the
// operator re-applies it. Krill never applies the firewall by itself.
func (s *Server) linkFirewallStale(r *http.Request) bool {
	if s.cpFirewall == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), firewallStatusTimeout)
	defer cancel()
	ips, err := s.clusterIPs(r)
	if err != nil {
		return false
	}
	pr, err := s.panelRow(ctx)
	if err != nil {
		return false
	}
	lp, err := s.linkPort(ctx)
	if err != nil {
		return false
	}
	ports, err := controlPlaneServicePorts(s.cfg.ListenAddr, pr.DirectPortClosed, lp)
	if err != nil {
		return false
	}
	cpAddr, _, all := s.swarmAddrs(ctx)
	rs, h, err := nodeRuleset(ctx, s.cpFirewall, cpAddr, firewall.Rules{ClusterIPs: ips, PublicTCP: ports.Public, GatewayTCP: ports.Gateway}, all)
	if err != nil {
		logFrom(r).Warn("linkPage: probe control-plane firewall failed", "err", err)
		return false
	}
	return h.Table && !h.Pending && h.LiveID != firewall.RulesetID(rs)
}

// setLinkRole switches the role: off, client, or relay with its link host.
func (s *Server) setLinkRole(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var err error
	switch r.FormValue("role") {
	case link.RoleOff:
		err = s.link.SetOff(ctx)
	case link.RoleClient:
		if key := relayRequirementKey(s.link.ListenRequirement()); key != "" {
			s.flashErrT(w, r, key)
			return
		}
		err = s.link.SetClient(ctx)
	case link.RoleRelay:
		if key := relayRequirementKey(s.link.RelayRequirement()); key != "" {
			s.flashErrT(w, r, key)
			return
		}
		host, herr := panel.NormalizeHost(r.FormValue("link_host"))
		if herr != nil {
			s.flashErrT(w, r, "link.err.invalid_host")
			return
		}
		err = s.link.SetRelay(ctx, host)
		var conflict *hostshare.ConflictError
		if errors.As(err, &conflict) {
			logFrom(r).Info("setLinkRole: link host taken", "host", host, "key", conflict.Key)
			s.flashErrT(w, r, conflict.Key)
			return
		}
	default:
		s.flashErrT(w, r, "link.err.invalid_role")
		return
	}
	if err != nil {
		logFrom(r).Error("setLinkRole: change failed", "role", r.FormValue("role"), "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link role set", "role", r.FormValue("role"))
	s.flashOK(w, r, "link.ok.role")
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}

// createLinkCode creates a pairing code for a new peer and shows it once.
func (s *Server) createLinkCode(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	peer := strings.TrimSpace(r.FormValue("peer"))
	if !link.ValidPeerName(peer) {
		s.flashErrT(w, r, "link.err.invalid_peer")
		return
	}
	code, err := s.link.CreatePairingCode(r.Context(), peer, r.FormValue("addr"))
	switch {
	case errors.Is(err, link.ErrWrongRole):
		s.flashErrT(w, r, "link.err.not_relay")
		return
	case errors.Is(err, link.ErrInvalidAddr):
		s.flashErrT(w, r, "link.err.invalid_addr")
		return
	case errors.Is(err, link.ErrPeerExists):
		s.flashErrT(w, r, "link.err.peer_exists")
		return
	case err != nil:
		logFrom(r).Error("createLinkCode: create failed", "peer", peer, "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link pairing code created", "peer", peer)
	s.setFlashLinkCode(w, r, code)
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}

// removeLinkPeer removes a peer and drops its session.
func (s *Server) removeLinkPeer(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "peerID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.link.RemovePeer(r.Context(), id); err != nil {
		if errors.Is(err, link.ErrPeerGone) {
			http.NotFound(w, r)
			return
		}
		logFrom(r).Error("removeLinkPeer: remove failed", "peer_id", id, "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link peer removed", "peer_id", id)
	s.flashOK(w, r, "link.ok.peer_removed")
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}

// pairLink pairs this client with a relay from a pasted code.
func (s *Server) pairLink(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	err := s.link.Pair(ctx, r.FormValue("code"))
	var refused *link.PairRefusedError
	var failed *link.PairFailedError
	switch {
	case err == nil:
	case errors.Is(err, link.ErrWrongRole):
		s.flashErrT(w, r, "link.err.not_client")
		return
	case errors.Is(err, link.ErrBadCode):
		s.flashErrT(w, r, "link.err.bad_code")
		return
	case errors.As(err, &refused):
		logFrom(r).Warn("pairLink: relay refused", "category", refused.Category)
		s.flashErrT(w, r, "link.err.pair_"+refused.Category)
		return
	case errors.As(err, &failed):
		logFrom(r).Warn("pairLink: relay unreachable", "category", failed.Category)
		s.flashErrT(w, r, "link.err.pair_"+failed.Category)
		return
	default:
		logFrom(r).Error("pairLink: pairing failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link paired with relay")
	s.flashOK(w, r, "link.ok.paired")
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}

// forgetLinkRelay erases the client's key and certificate.
func (s *Server) forgetLinkRelay(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.link == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.link.ForgetRelay(r.Context()); err != nil {
		logFrom(r).Error("forgetLinkRelay: forget failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("link relay forgotten")
	s.flashOK(w, r, "link.ok.forgotten")
	http.Redirect(w, r, linkBack(o.ID), http.StatusSeeOther)
}
