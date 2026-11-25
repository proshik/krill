package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/web/templates"
)

// pendingTimeout bounds what one page spends finding saved settings that are
// not live yet (each app costs a settings load; see deploy.PendingChanges).
const pendingTimeout = 3 * time.Second

// pendingChanges reports what the saved settings of the running apps among
// apps would change if deployed now. apps carry their live status: a stopped
// or never deployed app gets no entry (Start deploys the current settings
// anyway), nor does one being deployed or one deployed before Krill recorded
// what a deploy applies.
func (s *Server) pendingChanges(ctx context.Context, apps []db.Application) map[int64]templates.PendingView {
	out := map[int64]templates.PendingView{}
	if s.deployer == nil {
		return out
	}
	var ids []int64
	for _, a := range apps {
		if a.Status == deploy.StatusRunning {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, pendingTimeout)
	defer cancel()
	for id, p := range s.deployer.PendingChanges(ctx, ids) {
		out[id] = templates.PendingView{Changed: p.Changed, CanApply: p.CanApply}
	}
	return out
}

// applyAppSettings deploys the app's saved settings on the image it runs now,
// pinned by digest: no pull of its tag, no git clone, no build
// (deploy.Deployer.EnqueueApplySettings). Only a running app qualifies; a
// stopped one is started with Start, which deploys its current settings.
func (s *Server) applyAppSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if s.deployer == nil {
		s.flashErrT(w, r, "flash.err.queue_deploy")
		return
	}
	if s.engine != nil {
		st, err := s.engine.ServiceState(r.Context(), dockerName(c.App.ID))
		if err != nil {
			logFrom(r).Error("applyAppSettings: failed to read service state", "err", err, "app_id", c.App.ID)
			s.flashErrT(w, r, "flash.err.queue_deploy")
			return
		}
		if !st.Found || st.Desired == 0 {
			s.flashErrT(w, r, "flash.err.apply_stopped")
			return
		}
	}
	id, err := s.deployer.EnqueueApplySettings(r.Context(), c.App.ID, deploy.TriggerManual)
	if err != nil {
		logFrom(r).Warn("applyAppSettings: settings apply not accepted", "err", err, "app_id", c.App.ID)
		switch {
		case errors.Is(err, deploy.ErrNothingToApply):
			s.flashErrT(w, r, "flash.err.apply_nothing")
		case errors.Is(err, deploy.ErrApplyNeedsDeploy):
			s.flashErrT(w, r, "flash.err.apply_needs_deploy")
		default:
			s.flashErrT(w, r, deployRefusalKey(err, "flash.err.queue_deploy"))
		}
		return
	}
	logFrom(r).Info("settings apply enqueued", "app_id", c.App.ID, "app_name", c.App.Name, "deployment_id", id)
	s.flashOK(w, r, "flash.ok.apply_queued")
	http.Redirect(w, r, appURL(c)+"?tab=deployments", http.StatusSeeOther)
}
