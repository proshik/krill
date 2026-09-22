package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/proshik/krill/internal/appmetrics"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

func metricsTab(c templates.AppCtx) string { return appURL(c) + "?tab=metrics" }

func (s *Server) metricsFailure(w http.ResponseWriter, r *http.Request, err error) {
	logFrom(r).Error("app metrics action failed", "err", err)
	s.flashErrT(w, r, "flash.err.internal")
}

func (s *Server) metricsEnvTaken(ctx context.Context, app db.Application, name string) (bool, error) {
	links, err := s.q.ListDBLinksByApplication(ctx, app.ID)
	if err != nil {
		return false, err
	}
	vars := make([]string, 0, len(links))
	for _, link := range links {
		vars = append(vars, link.VarName)
	}
	return deploy.EnvNameInUse(app.EnvText, vars, name), nil
}

func (s *Server) portPublished(ctx context.Context, appID int64, port int32) (bool, error) {
	n, err := s.q.CountTCPAppPortsByContainerPort(ctx, db.CountTCPAppPortsByContainerPortParams{ApplicationID: appID, ContainerPort: port})
	return n > 0, err
}

func (s *Server) metricsSuccess(w http.ResponseWriter, r *http.Request, c templates.AppCtx, key string, syncLabels bool) {
	if syncLabels {
		s.syncAppLabels(r, c.App.ID, c.App.Port)
	}
	logFrom(r).Info("app metrics settings changed", "app_id", c.App.ID, "action", key)
	s.flashOK(w, r, key)
	http.Redirect(w, r, metricsTab(c), http.StatusSeeOther)
}

func (s *Server) enableAppMetrics(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := s.q.GetApplicationMetrics(ctx, c.App.ID)
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	taken, err := s.metricsEnvTaken(ctx, c.App, m.MetricsTokenEnv)
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	if taken {
		s.flashErr(w, r, i18n.Tf(ctx, "flash.err.metrics_env_taken", m.MetricsTokenEnv))
		return
	}
	eps, err := s.q.ListMetricsEndpointsByApplication(ctx, c.App.ID)
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	ports := []int32{c.App.Port}
	if len(eps) > 0 {
		ports = nil
		for _, ep := range eps {
			ports = append(ports, ep.Port)
		}
	}
	for _, port := range ports {
		pub, err := s.portPublished(ctx, c.App.ID, port)
		if err != nil {
			s.metricsFailure(w, r, err)
			return
		}
		if pub {
			s.flashErr(w, r, i18n.Tf(ctx, "flash.err.metrics_port_published", port))
			return
		}
	}
	if m.MetricsToken == nil {
		token, err := appmetrics.NewToken()
		if err != nil {
			s.metricsFailure(w, r, err)
			return
		}
		encrypted := secret.Enc(token)
		if err := s.q.SetApplicationMetricsToken(ctx, db.SetApplicationMetricsTokenParams{ID: c.App.ID, MetricsToken: &encrypted}); err != nil {
			s.metricsFailure(w, r, err)
			return
		}
	}
	if len(eps) == 0 {
		if _, err := s.q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: c.App.ID, Port: c.App.Port, Path: appmetrics.DefaultPath}); err != nil {
			s.metricsFailure(w, r, err)
			return
		}
	}
	if err := s.q.SetApplicationMetricsEnabled(ctx, db.SetApplicationMetricsEnabledParams{ID: c.App.ID, MetricsEnabled: true}); err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	s.metricsSuccess(w, r, c, "flash.ok.metrics_enabled", true)
}

func (s *Server) disableAppMetrics(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if err := s.q.SetApplicationMetricsEnabled(r.Context(), db.SetApplicationMetricsEnabledParams{ID: c.App.ID}); err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	s.metricsSuccess(w, r, c, "flash.ok.metrics_disabled", true)
}

func (s *Server) addAppMetricsEndpoint(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	port, err := strconv.Atoi(r.FormValue("port"))
	if err != nil || !appmetrics.ValidPort(port) {
		s.flashErrT(w, r, "flash.err.invalid_port")
		return
	}
	path, ok := appmetrics.NormalizePath(r.FormValue("path"))
	if !ok {
		s.flashErrT(w, r, "flash.err.metrics_invalid_path")
		return
	}
	job := strings.TrimSpace(r.FormValue("job"))
	if !appmetrics.ValidJob(job) {
		s.flashErrT(w, r, "flash.err.metrics_invalid_job")
		return
	}
	n, err := s.q.CountMetricsEndpointsByApplication(ctx, c.App.ID)
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	if n >= appmetrics.MaxEndpoints {
		s.flashErrT(w, r, "flash.err.metrics_endpoints_limit")
		return
	}
	pub, err := s.portPublished(ctx, c.App.ID, int32(port))
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	if pub {
		s.flashErr(w, r, i18n.Tf(ctx, "flash.err.metrics_port_published", port))
		return
	}
	_, err = s.q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: c.App.ID, Port: int32(port), Path: path, Job: job})
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			s.flashErrT(w, r, "flash.err.metrics_endpoint_exists")
		} else {
			s.metricsFailure(w, r, err)
		}
		return
	}
	s.metricsSuccess(w, r, c, "flash.ok.metrics_endpoint_added", true)
}

func (s *Server) deleteAppMetricsEndpoint(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r, "epID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ep, err := s.q.GetMetricsEndpoint(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ep.ApplicationID != c.App.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	if err := s.q.DeleteMetricsEndpoint(r.Context(), db.DeleteMetricsEndpointParams{ID: id, ApplicationID: c.App.ID}); err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	s.metricsSuccess(w, r, c, "flash.ok.metrics_endpoint_removed", true)
}

func (s *Server) setAppMetricsTokenEnv(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !appmetrics.ValidEnvName(name) {
		s.flashErrT(w, r, "flash.err.invalid_var_name")
		return
	}
	taken, err := s.metricsEnvTaken(r.Context(), c.App, name)
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	if taken {
		s.flashErr(w, r, i18n.Tf(r.Context(), "flash.err.metrics_env_taken", name))
		return
	}
	if err := s.q.SetApplicationMetricsTokenEnv(r.Context(), db.SetApplicationMetricsTokenEnvParams{ID: c.App.ID, MetricsTokenEnv: name}); err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	s.metricsSuccess(w, r, c, "flash.ok.metrics_env_saved", false)
}

func (s *Server) rotateAppMetricsToken(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	token, err := appmetrics.NewToken()
	if err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	encrypted := secret.Enc(token)
	if err := s.q.SetApplicationMetricsToken(r.Context(), db.SetApplicationMetricsTokenParams{ID: c.App.ID, MetricsToken: &encrypted}); err != nil {
		s.metricsFailure(w, r, err)
		return
	}
	s.metricsSuccess(w, r, c, "flash.ok.metrics_token_rotated", false)
}
