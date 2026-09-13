package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/web/templates"
)

// panelGateway is what the server needs to route its own UI through the
// gateway. The zero value is "not wired": no request counts as proxied and the
// panel page reports the feature unavailable.
type panelGateway struct {
	tokens      panel.Tokens
	upstream    string // "" when upstreamErr is set
	upstreamErr error
	// closeIdleConns drops the HTTP server's idle keep-alive connections, so a
	// request that follows a firewall change arrives over a connection the new
	// ruleset had to admit. Nil in tests.
	closeIdleConns func()
	// checkCert verifies the gateway serves a trusted certificate for a host.
	checkCert func(ctx context.Context, host string) error
}

// SetPanelGateway wires the panel domain. upstream/upstreamErr come from
// panel.Upstream; with an error the page explains why the feature is off.
func (s *Server) SetPanelGateway(tokens panel.Tokens, upstream string, upstreamErr error) {
	s.panel.tokens = tokens
	s.panel.upstream = upstream
	s.panel.upstreamErr = upstreamErr
	if s.panel.checkCert == nil {
		addr := s.cfg.AdvertiseAddr
		s.panel.checkCert = func(ctx context.Context, host string) error {
			return panel.CheckCertificate(ctx, addr, host)
		}
	}
}

// SetPanelCertCheck replaces the certificate check (tests).
func (s *Server) SetPanelCertCheck(fn func(ctx context.Context, host string) error) {
	s.panel.checkCert = fn
}

// SetIdleConnCloser wires the hook that drops idle keep-alive connections.
func (s *Server) SetIdleConnCloser(fn func()) { s.panel.closeIdleConns = fn }

// viaGateway reports whether r was proxied by Krill's own gateway.
func (s *Server) viaGateway(r *http.Request) bool {
	return panel.ViaGateway(r, s.panel.tokens.Forwarded)
}

// secureViaGateway reports whether r reached the gateway over HTTPS.
func (s *Server) secureViaGateway(r *http.Request) bool {
	return panel.SecureViaGateway(r, s.panel.tokens.Forwarded)
}

// trustForwardedFor decides whether r's X-Forwarded-For names the client.
func (s *Server) trustForwardedFor(r *http.Request) bool {
	return s.cfg.TrustProxy || s.viaGateway(r)
}

// cookieSecure decides the Secure flag of the cookies set on r's response.
// A request that reached the gateway over HTTPS gets secure cookies without any
// setting: its browser only ever talks HTTPS to that host. A request straight
// to the UI port keeps them off, which is what lets the operator still sign in
// at http://<ip>:8080 while the domain is being set up.
// KRILL_COOKIE_SECURE=true forces them on everywhere (an external TLS proxy).
func (s *Server) cookieSecure(r *http.Request) bool {
	return s.cfg.CookieSecure || s.secureViaGateway(r)
}

// panelRow reads the panel settings. A missing row is an instance where the
// gateway secret was never initialized (tests; main creates it at startup) and
// reads as "off".
func (s *Server) panelRow(ctx context.Context) (db.PanelGateway, error) {
	row, err := s.q.GetPanelGateway(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PanelGateway{State: panel.StateOff}, nil
	}
	return row, err
}

// baseURL is the externally reachable base URL shown in webhook addresses:
// KRILL_PUBLIC_URL, else the confirmed panel domain, else the legacy derivation
// from KRILL_HOST.
func (s *Server) baseURL(ctx context.Context) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	if row, err := s.panelRow(ctx); err == nil && row.State == panel.StateActive && row.Host != "" {
		return "https://" + row.Host
	}
	return s.cfg.BaseURL()
}

// hostReservedByPanel reports whether host is the panel's domain, which no app
// may claim: its router would compete with the panel's for the admin's login.
func (s *Server) hostReservedByPanel(ctx context.Context, host string) (bool, error) {
	row, err := s.panelRow(ctx)
	if err != nil {
		return false, err
	}
	return row.State != panel.StateOff && strings.EqualFold(row.Host, host), nil
}

// gatewayConfig serves the panel's dynamic configuration to the gateway's HTTP
// provider. Anything but a poll carrying the provider token gets a 404, and so
// does a request the gateway itself proxied: the forwarded token is on every
// request through the panel's domain, and the configuration holds it.
//
// A read error answers 500, never an empty configuration: the provider keeps
// the routes it has on a failed poll, while an empty answer would take the
// panel's domain down over a transient database error.
func (s *Server) gatewayConfig(w http.ResponseWriter, r *http.Request) {
	if !panel.Matches(r.Header.Get(panel.ProviderHeader), s.panel.tokens.Provider) || s.viaGateway(r) {
		http.NotFound(w, r)
		return
	}
	row, err := s.panelRow(r.Context())
	if err != nil {
		logFrom(r).Error("gatewayConfig: read panel settings failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cfg := panel.DynamicConfig(panel.SettingsFromRow(row), s.panel.upstream, s.panel.tokens.Forwarded)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		logFrom(r).Warn("gatewayConfig: write failed", "err", err)
	}
}

// panelBack is the panel page of the organization in the request path.
func panelBack(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/panel-domain"
}

// onPanelDomain reports whether r reached this instance through the panel's
// domain over HTTPS — the only kind of request that proves the domain works.
func (s *Server) onPanelDomain(r *http.Request, row db.PanelGateway) bool {
	return row.Host != "" && s.secureViaGateway(r) && panel.RequestHost(r) == row.Host
}

// panelDomainPage renders the panel domain settings. Instance-admin only.
func (s *Server) panelDomainPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	row, err := s.panelRow(r.Context())
	if err != nil {
		logFrom(r).Error("panelDomainPage: read panel settings failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := templates.PanelDomainView{
		Available:        s.panel.tokens.Forwarded != "" && s.panel.upstreamErr == nil,
		Host:             row.Host,
		State:            row.State,
		AllowedIPs:       row.AllowedIps,
		OnDomain:         s.onPanelDomain(r, row),
		DirectPortClosed: row.DirectPortClosed,
		ClosePending:     row.DirectPortClosePending,
		FirewallWired:    s.cpFirewall != nil,
		ClientIP:         clientIP(r, s.trustForwardedFor(r)),
		DirectURL:        s.directURL(),
	}
	switch {
	case s.panel.tokens.Forwarded == "":
		v.UnavailableKey = "panel.unavailable.unwired"
	case errors.Is(s.panel.upstreamErr, panel.ErrAdvertiseUnset):
		v.UnavailableKey = "panel.unavailable.advertise"
	case errors.Is(s.panel.upstreamErr, panel.ErrListenLoopback):
		v.UnavailableKey = "panel.unavailable.loopback"
	case s.panel.upstreamErr != nil:
		v.UnavailableKey = "panel.unavailable.listen"
	}
	if row.Host != "" {
		v.DomainURL = "https://" + row.Host + panelBack(o.ID)
	}
	if s.cpFirewall != nil {
		st := s.controlPlaneFirewallState(r.Context())
		v.FirewallLocked = st.Available && st.Locked
		if row.DirectPortClosePending && st.Available && !st.Pending {
			// The dead-man switch fired before the close was confirmed: the
			// previous ruleset — with the UI port open — is back.
			if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{DirectPortClosed: false, DirectPortClosePending: false}); err != nil {
				logFrom(r).Error("panelDomainPage: clear reverted close failed", "err", err)
			} else {
				v.ClosePending = false
				v.CloseReverted = true
			}
		}
		if v.ClosePending {
			// See confirmControlPlaneFirewall: the confirmation has to come over
			// a connection opened after the ruleset changed.
			w.Header().Set("Connection", "close")
		}
	}
	render(w, r, http.StatusOK, templates.PanelDomain(o, role, v))
}

// directURL is the address the UI port answers on directly, for display.
func (s *Server) directURL() string {
	_, port, err := net.SplitHostPort(s.cfg.ListenAddr)
	if err != nil || s.cfg.AdvertiseAddr == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(s.cfg.AdvertiseAddr, port)
}

// setPanelDomain routes the panel on a domain, pending confirmation. The UI
// port keeps working exactly as before: nothing about direct access changes
// until an operator proves the domain from a browser.
func (s *Server) setPanelDomain(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.panel.tokens.Forwarded == "" || s.panel.upstreamErr != nil {
		s.flashErrT(w, r, "flash.err.panel_unavailable")
		return
	}
	host, err := panel.NormalizeHost(r.FormValue("host"))
	if err != nil {
		s.flashErrT(w, r, "flash.err.panel_invalid_host")
		return
	}
	row, err := s.panelRow(r.Context())
	if err != nil {
		logFrom(r).Error("setPanelDomain: read panel settings failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if row.Host == host && row.State != panel.StateOff {
		http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
		return
	}
	if row.DirectPortClosed || row.DirectPortClosePending {
		// Moving the domain would strand an operator who can reach the panel
		// through nothing else.
		s.flashErrT(w, r, "flash.err.panel_direct_closed")
		return
	}
	n, err := s.q.CountDomainsByHost(r.Context(), host)
	if err != nil {
		logFrom(r).Error("setPanelDomain: count app domains failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if n > 0 {
		s.flashErrT(w, r, "flash.err.panel_host_in_use")
		return
	}
	if err := s.q.SetPanelDomainPending(r.Context(), host); err != nil {
		logFrom(r).Error("setPanelDomain: save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("panel domain set, awaiting confirmation", "host", host)
	s.flashOK(w, r, "flash.ok.panel_pending")
	http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
}

// confirmPanelDomain marks the panel domain active. It accepts only a request
// that reached the panel through that domain over HTTPS: a check from this
// process would travel over loopback and prove nothing about the outside world.
// The certificate is checked separately, because a browser lets an operator
// click through Traefik's self-signed default.
func (s *Server) confirmPanelDomain(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	row, err := s.panelRow(r.Context())
	if err != nil {
		logFrom(r).Error("confirmPanelDomain: read panel settings failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if row.State != panel.StatePending {
		s.flashErrT(w, r, "flash.err.panel_not_pending")
		return
	}
	if !s.onPanelDomain(r, row) {
		s.flashErrT(w, r, "flash.err.panel_confirm_off_domain")
		return
	}
	if !s.cfg.AcmeStaging && s.panel.checkCert != nil {
		if err := s.panel.checkCert(r.Context(), row.Host); err != nil {
			logFrom(r).Warn("confirmPanelDomain: certificate not trusted yet", "host", row.Host, "err", err)
			s.flashErrT(w, r, "flash.err.panel_cert")
			return
		}
	}
	n, err := s.q.ActivatePanelDomain(r.Context(), row.Host)
	if err != nil {
		logFrom(r).Error("confirmPanelDomain: save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if n == 0 {
		s.flashErrT(w, r, "flash.err.panel_not_pending")
		return
	}
	logFrom(r).Info("panel domain confirmed", "host", row.Host)
	s.flashOK(w, r, "flash.ok.panel_active")
	http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
}

// disablePanelDomain removes the panel's route. Refused while the UI port is
// closed to the outside: the domain is then the only way in.
func (s *Server) disablePanelDomain(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	row, err := s.panelRow(r.Context())
	if err != nil {
		logFrom(r).Error("disablePanelDomain: read panel settings failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if row.DirectPortClosed || row.DirectPortClosePending {
		s.flashErrT(w, r, "flash.err.panel_direct_closed")
		return
	}
	if err := s.q.DisablePanelDomain(r.Context()); err != nil {
		logFrom(r).Error("disablePanelDomain: save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("panel domain disabled", "host", row.Host)
	s.flashOK(w, r, "flash.ok.panel_disabled")
	http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
}

// setPanelAllowedIPs restricts the browser UI on the panel domain to a list of
// addresses. An operator working through the domain cannot save a list that
// excludes the address they are working from.
func (s *Server) setPanelAllowedIPs(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	cidrs, err := validAllowedIPs(r.FormValue("allowed_ips"))
	if err != nil {
		s.flashErr(w, r, err.Error())
		return
	}
	if len(cidrs) > 0 && s.viaGateway(r) && !ipInCIDRs(clientIP(r, true), cidrs) {
		s.flashErrT(w, r, "flash.err.panel_allowlist_self")
		return
	}
	if err := s.q.SetPanelAllowedIPs(r.Context(), strings.Join(cidrs, "\n")); err != nil {
		logFrom(r).Error("setPanelAllowedIPs: save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("panel allowlist updated", "entries", len(cidrs))
	s.flashOK(w, r, "flash.ok.panel_allowlist")
	http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
}

// ipInCIDRs reports whether ip falls inside any of cidrs.
func ipInCIDRs(ip string, cidrs []string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(parsed) {
			return true
		}
	}
	return false
}

// closePanelDirectPort closes the UI port to the outside world, leaving it
// reachable only from the gateway. It is a change to the control-plane
// firewall, so it goes through the same dead-man switch as the lockdown, and it
// has to be started from the panel domain — the page the operator will need to
// confirm from once the port is gone.
func (s *Server) closePanelDirectPort(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.cpFirewall == nil {
		http.NotFound(w, r)
		return
	}
	row, err := s.panelRow(r.Context())
	if err != nil {
		logFrom(r).Error("closePanelDirectPort: read panel settings failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if row.State != panel.StateActive || !s.onPanelDomain(r, row) {
		s.flashErrT(w, r, "flash.err.panel_close_off_domain")
		return
	}
	if row.DirectPortClosed {
		http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
		return
	}
	locked, err := firewall.Status(r.Context(), s.cpFirewall)
	if err != nil {
		logFrom(r).Warn("closePanelDirectPort: firewall status unavailable", "err", err)
		s.flashErrT(w, r, "flash.err.panel_firewall_unavailable")
		return
	}
	if !locked {
		s.flashErrT(w, r, "flash.err.panel_firewall_open")
		return
	}
	ruleset, key := s.controlPlaneRuleset(r, true)
	if key != "" {
		s.flashErrT(w, r, key)
		return
	}
	if key := s.lockdownControlPlane(r, ruleset); key != "" {
		s.flashErrT(w, r, key)
		return
	}
	if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{DirectPortClosed: false, DirectPortClosePending: true}); err != nil {
		// The switch is armed and nothing will confirm it: it reverts on its own.
		logFrom(r).Error("closePanelDirectPort: save pending close failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("panel direct port closed; awaiting confirmation through the domain")
	s.flashOK(w, r, "flash.ok.panel_close_pending")
	w.Header().Set("Connection", "close")
	http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
}

// confirmPanelDirectPort keeps the closed UI port. Like the lockdown's own
// confirmation, reaching this handler through the domain after the ruleset
// changed is the proof that the gateway can still reach Krill.
func (s *Server) confirmPanelDirectPort(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.cpFirewall == nil {
		http.NotFound(w, r)
		return
	}
	row, err := s.panelRow(r.Context())
	if err != nil {
		logFrom(r).Error("confirmPanelDirectPort: read panel settings failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if !row.DirectPortClosePending {
		s.flashErrT(w, r, "flash.err.firewall_cp_not_pending")
		return
	}
	if !s.onPanelDomain(r, row) {
		s.flashErrT(w, r, "flash.err.panel_close_off_domain")
		return
	}
	pending, err := firewall.RevertPending(r.Context(), s.cpFirewall)
	if err != nil {
		logFrom(r).Error("confirmPanelDirectPort: read revert state failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if !pending {
		if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{}); err != nil {
			logFrom(r).Error("confirmPanelDirectPort: clear reverted close failed", "err", err)
		}
		s.flashErrT(w, r, "flash.err.firewall_cp_not_pending")
		return
	}
	if err := firewall.Confirm(r.Context(), s.cpFirewall); err != nil {
		logFrom(r).Error("confirmPanelDirectPort: confirm failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{DirectPortClosed: true}); err != nil {
		logFrom(r).Error("confirmPanelDirectPort: save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("panel direct port close confirmed")
	s.flashOK(w, r, "flash.ok.panel_close_confirmed")
	http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
}

// openPanelDirectPort makes the UI port public again. Loosening a ruleset
// cannot lock anyone out, so it is confirmed right away.
func (s *Server) openPanelDirectPort(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if s.cpFirewall == nil {
		http.NotFound(w, r)
		return
	}
	locked, err := firewall.Status(r.Context(), s.cpFirewall)
	if err != nil {
		logFrom(r).Warn("openPanelDirectPort: firewall status unavailable", "err", err)
		s.flashErrT(w, r, "flash.err.panel_firewall_unavailable")
		return
	}
	if locked {
		ruleset, key := s.controlPlaneRuleset(r, false)
		if key != "" {
			s.flashErrT(w, r, key)
			return
		}
		if err := firewall.Apply(r.Context(), s.cpFirewall, ruleset); err != nil {
			logFrom(r).Warn("openPanelDirectPort: apply failed", "err", err)
			s.flashErrT(w, r, "flash.err.firewall_cp_apply")
			return
		}
		if err := firewall.Confirm(r.Context(), s.cpFirewall); err != nil {
			logFrom(r).Error("openPanelDirectPort: confirm failed", "err", err)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
	}
	if err := s.q.SetPanelDirectPort(r.Context(), db.SetPanelDirectPortParams{}); err != nil {
		logFrom(r).Error("openPanelDirectPort: save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("panel direct port opened")
	s.flashOK(w, r, "flash.ok.panel_direct_opened")
	http.Redirect(w, r, panelBack(o.ID), http.StatusSeeOther)
}
