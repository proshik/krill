package server

import (
	"net/http"
	"regexp"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

func validHost(h string) bool {
	h = strings.ToLower(strings.TrimSpace(h))
	return len(h) <= 253 && hostRe.MatchString(h)
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
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	if n, _ := s.q.CountDomainsByHost(r.Context(), host); n > 0 {
		logFrom(r).Info("addDomain: host already in use", "app_id", c.App.ID, "host", host)
		http.Error(w, "host already in use", http.StatusBadRequest)
		return
	}
	if _, err := s.q.CreateDomain(r.Context(), db.CreateDomainParams{
		ApplicationID: c.App.ID, Host: host, Tls: tls, IsPrimary: false,
	}); err != nil {
		logFrom(r).Error("addDomain: create failed", "err", err, "app_id", c.App.ID, "host", host)
		http.Error(w, "failed to add domain", http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("domain added", "app_id", c.App.ID, "host", host, "tls", tls)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
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
		http.Error(w, "failed to update domain", http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("domain tls toggled", "domain_id", d.ID, "app_id", c.App.ID, "tls", !d.Tls)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
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
	if n, _ := s.q.CountDomainsByApplication(r.Context(), c.App.ID); n <= 1 {
		logFrom(r).Info("deleteDomain: refused to delete last domain", "app_id", c.App.ID, "domain_id", d.ID)
		http.Error(w, "cannot delete the last domain", http.StatusBadRequest)
		return
	}
	if err := s.q.DeleteDomain(r.Context(), d.ID); err != nil {
		logFrom(r).Error("deleteDomain: delete failed", "err", err, "domain_id", d.ID)
		http.Error(w, "failed to delete domain", http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("domain deleted", "domain_id", d.ID, "app_id", c.App.ID, "host", d.Host)
	s.syncAppLabels(r, c.App.ID, c.App.Port)
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
		ds = append(ds, traefik.Domain{Host: d.Host, TLS: d.Tls})
	}
	labels := traefik.AppLabels(docker.ServiceName(appID), ds, port, s.cfg.Network)
	if err := s.engine.ServiceUpdateLabels(r.Context(), docker.ServiceName(appID), labels); err != nil {
		logFrom(r).Error("syncAppLabels: update labels failed", "err", err, "app_id", appID)
	}
}
