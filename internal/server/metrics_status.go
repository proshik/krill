package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/proshik/krill/internal/appmetrics"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

func (s *Server) appMetricsStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	v := templates.MetricsStatus{}
	problem := func(key string, args ...any) {
		v.Problem = i18n.Tf(r.Context(), key, args...)
		render(w, r, 200, templates.AppMetricsStatus(v))
	}
	if !c.App.MetricsEnabled {
		problem("metrics.status.off")
		return
	}
	row, err := s.q.GetObservabilitySettings(r.Context())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logFrom(r).Error("appMetricsStatus: settings failed", "err", err)
		problem("metrics.status.unavailable", i18n.T(r.Context(), "flash.err.internal"))
		return
	}
	if !row.Enabled || row.MetricsUrl == "" {
		problem("metrics.status.obs_off")
		return
	}
	if s.obs.ctl == nil {
		problem("metrics.status.unwired")
		return
	}
	st := s.obs.ctl.Status(r.Context())
	if st.AppsErr != "" {
		problem("metrics.status.unavailable", st.AppsErr)
		return
	}
	if !st.Apps.Found || st.Apps.Running == 0 {
		problem("metrics.status.collector_down", st.LastErr)
		return
	}
	poll := s.appsProviderLastPoll()
	if poll.IsZero() {
		problem("metrics.status.never_polled")
		return
	}
	if time.Since(poll) > 2*time.Minute {
		problem("metrics.status.poll_stale", time.Since(poll).Round(time.Second).String())
		return
	}
	if inspector, ok := s.engine.(docker.ServiceInspector); ok {
		labels, found, err := inspector.ServiceContainerLabels(r.Context(), docker.ServiceName(c.App.ID))
		if err != nil {
			problem("metrics.status.unavailable", err.Error())
			return
		}
		if !found {
			problem("metrics.status.not_deployed")
			return
		}
		if c.App.MetricsToken == nil {
			problem("metrics.status.redeploy")
			return
		}
		token, err := secret.Dec(*c.App.MetricsToken)
		if err != nil {
			problem("metrics.token_unreadable")
			return
		}
		if labels[appmetrics.ContainerLabelTokenHash] != appmetrics.TokenHash(c.App.MetricsTokenEnv, token) {
			problem("metrics.status.redeploy")
			return
		}
	}
	if s.engine == nil {
		problem("metrics.status.unavailable", "Docker engine is unavailable")
		return
	}
	// Stop scales the service to zero and records the app as idle, so only the
	// live service can tell a stopped app apart from one that is still starting.
	state, err := s.engine.ServiceState(r.Context(), docker.ServiceName(c.App.ID))
	if err != nil {
		problem("metrics.status.unavailable", err.Error())
		return
	}
	if state.Found && state.Desired == 0 && state.Running == 0 {
		problem("metrics.status.app_stopped")
		return
	}
	eps, err := s.q.ListMetricsEndpointsByApplication(r.Context(), c.App.ID)
	if err != nil {
		logFrom(r).Error("appMetricsStatus: endpoints failed", "err", err)
		problem("metrics.status.unavailable", i18n.T(r.Context(), "flash.err.internal"))
		return
	}
	ids := make([]int64, 0, len(eps))
	for _, ep := range eps {
		ids = append(ids, ep.ID)
	}
	health, statuses, err := observability.ReadAppsStatus(r.Context(), func(ctx context.Context, svc string, cmd []string, out io.Writer) error {
		return s.engine.Exec(ctx, svc, cmd, nil, nil, out)
	}, c.App.ID, ids)
	if err != nil {
		problem("metrics.status.unavailable", err.Error())
		return
	}
	if !health.Healthy {
		problem("metrics.status.module_error", health.Message)
		return
	}
	v.Endpoints = statuses
	render(w, r, 200, templates.AppMetricsStatus(v))
}
