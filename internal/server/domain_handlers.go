package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

func validHost(h string) bool {
	h = strings.ToLower(strings.TrimSpace(h))
	return len(h) <= 253 && hostRe.MatchString(h)
}

// validPaths parses a newline-separated paths textarea: each entry must start
// with "/" and contain no whitespace or backticks (safe to interpolate into a
// Traefik rule). Returns the cleaned prefixes (empty slice = all paths).
func validPaths(raw string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "` \t") {
			return nil, errors.New("each path must start with / and contain no spaces or backticks")
		}
		out = append(out, p)
	}
	return out, nil
}

var basicAuthUserRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validBasicAuthUser checks a basic-auth username: no ":" (htpasswd separator),
// no "," (Traefik list separator), no whitespace — so it cannot inject into the
// label value.
func validBasicAuthUser(u string) bool { return basicAuthUserRe.MatchString(u) }

// validAllowedIPs parses a newline-separated list of CIDRs/IPs. A bare IP is
// normalized to /32 (v4) or /128 (v6). Any invalid line rejects the whole input,
// so only valid CIDRs ever reach the Traefik sourcerange.
func validAllowedIPs(raw string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(s); err == nil {
			out = append(out, s)
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP or CIDR: %s", s)
		}
		if ip.To4() != nil {
			out = append(out, s+"/32")
		} else {
			out = append(out, s+"/128")
		}
	}
	return out, nil
}

// basicAuthEntries splits stored newline-separated "user:hash" entries.
func basicAuthEntries(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if e := strings.TrimSpace(line); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// loadDomain loads {domainID}, verifying it belongs to the app in the chain.
func (s *Server) loadDomain(w http.ResponseWriter, r *http.Request, appID int64) (db.Domain, bool) {
	id, ok := pathID(r, "domainID")
	if !ok {
		http.NotFound(w, r)
		return db.Domain{}, false
	}
	d, err := s.q.GetDomain(r.Context(), id)
	if err != nil || d.ApplicationID != appID {
		logFrom(r).Info("loadDomain: not found or app mismatch", "domain_id", id, "app_id", appID)
		http.NotFound(w, r)
		return db.Domain{}, false
	}
	return d, true
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	host := strings.ToLower(strings.TrimSpace(r.FormValue("host")))
	tls := r.FormValue("tls") == "on"
	if !validHost(host) {
		logFrom(r).Info("addDomain: invalid host", "app_id", c.App.ID, "host", host)
		s.flashErr(w, r, "invalid host")
		return
	}
	if n, _ := s.q.CountDomainsByHost(r.Context(), host); n > 0 {
		logFrom(r).Info("addDomain: host already in use", "app_id", c.App.ID, "host", host)
		s.flashErr(w, r, "host already in use")
		return
	}
	expose := r.FormValue("exposed") == "on"
	paths, perr := validPaths(r.FormValue("paths"))
	if perr != nil {
		s.flashErr(w, r, perr.Error())
		return
	}
	if _, err := s.q.CreateDomain(r.Context(), db.CreateDomainParams{
		ApplicationID: c.App.ID, Host: host, Tls: tls, IsPrimary: false, Exposed: expose, Paths: strings.Join(paths, "\n"),
	}); err != nil {
		logFrom(r).Error("addDomain: create failed", "err", err, "app_id", c.App.ID, "host", host)
		s.flashErr(w, r, "failed to add domain")
		return
	}
	logFrom(r).Info("domain added", "app_id", c.App.ID, "host", host, "tls", tls)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
	s.setFlash(w, "ok", "Domain added")
	http.Redirect(w, r, appURL(c)+"?tab=domains", http.StatusSeeOther)
}

func (s *Server) toggleDomainTLS(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	d, ok := s.loadDomain(w, r, c.App.ID)
	if !ok {
		return
	}
	if err := s.q.SetDomainTLS(r.Context(), db.SetDomainTLSParams{ID: d.ID, Tls: !d.Tls}); err != nil {
		logFrom(r).Error("toggleDomainTLS: update failed", "err", err, "domain_id", d.ID)
		s.flashErr(w, r, "failed to update domain")
		return
	}
	logFrom(r).Info("domain tls toggled", "domain_id", d.ID, "app_id", c.App.ID, "tls", !d.Tls)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
	s.setFlash(w, "ok", "TLS setting updated")
	http.Redirect(w, r, appURL(c)+"?tab=domains", http.StatusSeeOther)
}

func (s *Server) setDomainExposure(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	d, ok := s.loadDomain(w, r, c.App.ID)
	if !ok {
		return
	}
	expose := r.FormValue("exposed") == "on"
	paths, perr := validPaths(r.FormValue("paths"))
	if perr != nil {
		s.flashErr(w, r, perr.Error())
		return
	}
	if err := s.q.UpdateDomainExposure(r.Context(), db.UpdateDomainExposureParams{
		ID: d.ID, Exposed: expose, Paths: strings.Join(paths, "\n"),
	}); err != nil {
		logFrom(r).Error("setDomainExposure: update failed", "err", err, "domain_id", d.ID)
		s.flashErr(w, r, "failed to update exposure")
		return
	}
	logFrom(r).Info("domain exposure updated", "domain_id", d.ID, "app_id", c.App.ID, "exposed", expose, "paths", len(paths))
	s.syncAppLabels(r, c.App.ID, c.App.Port)
	s.setFlash(w, "ok", "Exposure updated")
	http.Redirect(w, r, appURL(c)+"?tab=domains", http.StatusSeeOther)
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	d, ok := s.loadDomain(w, r, c.App.ID)
	if !ok {
		return
	}
	if d.IsPrimary {
		logFrom(r).Info("deleteDomain: refused to delete primary domain", "app_id", c.App.ID, "domain_id", d.ID)
		s.flashErr(w, r, "cannot delete the primary domain")
		return
	}
	if n, _ := s.q.CountDomainsByApplication(r.Context(), c.App.ID); n <= 1 {
		logFrom(r).Info("deleteDomain: refused to delete last domain", "app_id", c.App.ID, "domain_id", d.ID)
		s.flashErr(w, r, "cannot delete the last domain")
		return
	}
	if err := s.q.DeleteDomain(r.Context(), d.ID); err != nil {
		logFrom(r).Error("deleteDomain: delete failed", "err", err, "domain_id", d.ID)
		s.flashErr(w, r, "failed to delete domain")
		return
	}
	logFrom(r).Info("domain deleted", "domain_id", d.ID, "app_id", c.App.ID, "host", d.Host)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
	s.setFlash(w, "ok", "Domain deleted")
	http.Redirect(w, r, appURL(c)+"?tab=domains", http.StatusSeeOther)
}

// syncAppLabels re-applies the application's Traefik labels (no container restart).
func (s *Server) syncAppLabels(r *http.Request, appID int64, port int32) {
	if s.engine == nil {
		return
	}
	doms, err := s.q.ListDomainsByApplication(r.Context(), appID)
	if err != nil {
		logFrom(r).Error("syncAppLabels: list domains failed", "err", err, "app_id", appID)
		return
	}
	ds := make([]traefik.Domain, 0, len(doms))
	for _, d := range doms {
		ds = append(ds, traefik.Domain{
			Host: d.Host, TLS: d.Tls, Exposed: d.Exposed, Paths: traefik.SplitPaths(d.Paths),
			BasicAuthUsers: traefik.SplitPaths(d.BasicAuthUsers), AllowedIPs: traefik.SplitPaths(d.AllowedIps),
		})
	}
	labels := traefik.AppLabels(docker.ServiceName(appID), ds, port, s.cfg.Network)
	if err := s.engine.ServiceUpdateLabels(r.Context(), docker.ServiceName(appID), labels); err != nil {
		logFrom(r).Error("syncAppLabels: update labels failed", "err", err, "app_id", appID)
	}
}

// addDomainBasicAuthUser appends a basic-auth user (username + bcrypt hash) to a
// domain's htpasswd list and re-applies labels.
func (s *Server) addDomainBasicAuthUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	d, ok := s.loadDomain(w, r, c.App.ID)
	if !ok {
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if !validBasicAuthUser(username) || password == "" {
		s.flashErr(w, r, "invalid username or empty password")
		return
	}
	entries := basicAuthEntries(d.BasicAuthUsers)
	for _, e := range entries {
		if strings.SplitN(e, ":", 2)[0] == username {
			s.flashErr(w, r, "username already exists")
			return
		}
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		logFrom(r).Error("addDomainBasicAuthUser: hash failed", "err", err, "domain_id", d.ID)
		s.flashErr(w, r, "internal error")
		return
	}
	entries = append(entries, username+":"+hash)
	if err := s.q.SetDomainBasicAuth(r.Context(), db.SetDomainBasicAuthParams{ID: d.ID, BasicAuthUsers: strings.Join(entries, "\n")}); err != nil {
		logFrom(r).Error("addDomainBasicAuthUser: update failed", "err", err, "domain_id", d.ID)
		s.flashErr(w, r, "failed to add user")
		return
	}
	logFrom(r).Info("domain basic-auth user added", "domain_id", d.ID, "app_id", c.App.ID, "username", username)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
	s.setFlash(w, "ok", "Basic-auth user added")
	http.Redirect(w, r, appURL(c)+"?tab=domains", http.StatusSeeOther)
}

// deleteDomainBasicAuthUser removes a basic-auth user by username and re-applies labels.
func (s *Server) deleteDomainBasicAuthUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	d, ok := s.loadDomain(w, r, c.App.ID)
	if !ok {
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	kept := make([]string, 0)
	for _, e := range basicAuthEntries(d.BasicAuthUsers) {
		if strings.SplitN(e, ":", 2)[0] != username {
			kept = append(kept, e)
		}
	}
	if err := s.q.SetDomainBasicAuth(r.Context(), db.SetDomainBasicAuthParams{ID: d.ID, BasicAuthUsers: strings.Join(kept, "\n")}); err != nil {
		logFrom(r).Error("deleteDomainBasicAuthUser: update failed", "err", err, "domain_id", d.ID)
		s.flashErr(w, r, "failed to remove user")
		return
	}
	logFrom(r).Info("domain basic-auth user removed", "domain_id", d.ID, "app_id", c.App.ID, "username", username)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
	s.setFlash(w, "ok", "Basic-auth user removed")
	http.Redirect(w, r, appURL(c)+"?tab=domains", http.StatusSeeOther)
}

// setDomainAllowedIPs replaces a domain's IP-allowlist (CIDR/IP) and re-applies labels.
func (s *Server) setDomainAllowedIPs(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	d, ok := s.loadDomain(w, r, c.App.ID)
	if !ok {
		return
	}
	ips, err := validAllowedIPs(r.FormValue("ips"))
	if err != nil {
		s.flashErr(w, r, err.Error())
		return
	}
	if err := s.q.SetDomainAllowedIPs(r.Context(), db.SetDomainAllowedIPsParams{ID: d.ID, AllowedIps: strings.Join(ips, "\n")}); err != nil {
		logFrom(r).Error("setDomainAllowedIPs: update failed", "err", err, "domain_id", d.ID)
		s.flashErr(w, r, "failed to update allowed IPs")
		return
	}
	logFrom(r).Info("domain allowed IPs updated", "domain_id", d.ID, "app_id", c.App.ID, "count", len(ips))
	s.syncAppLabels(r, c.App.ID, c.App.Port)
	s.setFlash(w, "ok", "Allowed IPs updated")
	http.Redirect(w, r, appURL(c)+"?tab=domains", http.StatusSeeOther)
}
