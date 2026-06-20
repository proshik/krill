package server

import (
	"net/http"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
)

// addAppPort publishes a raw host port into the app container (host mode),
// bypassing Traefik. The mapping is applied on the next deploy (the ServiceSpec
// is rebuilt from app_ports). Host ports are treated as globally unique across
// apps and managed-DB external ports.
func (s *Server) addAppPort(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	hostPort, herr := strconv.Atoi(r.FormValue("host_port"))
	containerPort, cerr := strconv.Atoi(r.FormValue("container_port"))
	if herr != nil || cerr != nil || hostPort < 1 || hostPort > 65535 || containerPort < 1 || containerPort > 65535 {
		s.flashErrT(w, r, "flash.err.invalid_port")
		return
	}
	protocol := r.FormValue("protocol")
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		s.flashErrT(w, r, "flash.err.invalid_protocol")
		return
	}
	// Conflict: another app already publishes this host port on the same protocol.
	if n, err := s.q.CountAppPortsByHostPort(r.Context(), db.CountAppPortsByHostPortParams{
		HostPort: int32(hostPort), Protocol: protocol,
	}); err != nil {
		logFrom(r).Error("addAppPort: count app ports failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	} else if n > 0 {
		s.flashErrT(w, r, "flash.err.port_in_use")
		return
	}
	// Cross-check against managed-DB external ports (those are TCP host ports).
	if protocol == "tcp" {
		hp := int32(hostPort)
		pgN, perr := s.q.CountPostgresByExternalPort(r.Context(), &hp)
		rdN, rerr := s.q.CountRedisByExternalPort(r.Context(), &hp)
		if perr != nil || rerr != nil {
			logFrom(r).Error("addAppPort: count db external ports failed", "err_pg", perr, "err_redis", rerr, "app_id", c.App.ID)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		if pgN > 0 || rdN > 0 {
			s.flashErrT(w, r, "flash.err.port_in_use")
			return
		}
	}
	if _, err := s.q.CreateAppPort(r.Context(), db.CreateAppPortParams{
		ApplicationID: c.App.ID, HostPort: int32(hostPort), ContainerPort: int32(containerPort), Protocol: protocol,
	}); err != nil {
		logFrom(r).Error("addAppPort: create failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.port_in_use")
		return
	}
	logFrom(r).Info("app port published", "app_id", c.App.ID, "host_port", hostPort, "container_port", containerPort, "protocol", protocol)
	s.flashOK(w, r, "flash.ok.port_added")
	http.Redirect(w, r, appURL(c)+"?tab=advanced", http.StatusSeeOther)
}

// deleteAppPort removes a published port. It stops being published on the next
// deploy (the running service keeps the binding until then).
func (s *Server) deleteAppPort(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	p, ok := s.loadPort(w, r, c.App.ID)
	if !ok {
		return
	}
	if err := s.q.DeleteAppPort(r.Context(), p.ID); err != nil {
		logFrom(r).Error("deleteAppPort: delete failed", "err", err, "port_id", p.ID)
		s.flashErrT(w, r, "flash.err.delete_port")
		return
	}
	logFrom(r).Info("app port removed", "app_id", c.App.ID, "port_id", p.ID, "host_port", p.HostPort)
	s.flashOK(w, r, "flash.ok.port_removed")
	http.Redirect(w, r, appURL(c)+"?tab=advanced", http.StatusSeeOther)
}

// loadPort parses {portID} and verifies it belongs to the given application
// (tenancy is already checked by loadAppCtx). 404 on any mismatch.
func (s *Server) loadPort(w http.ResponseWriter, r *http.Request, appID int64) (db.AppPort, bool) {
	id, ok := pathID(r, "portID")
	if !ok {
		http.NotFound(w, r)
		return db.AppPort{}, false
	}
	p, err := s.q.GetAppPort(r.Context(), id)
	if err != nil || p.ApplicationID != appID {
		logFrom(r).Info("loadPort: not found in app", "port_id", id, "app_id", appID)
		http.NotFound(w, r)
		return db.AppPort{}, false
	}
	return p, true
}
