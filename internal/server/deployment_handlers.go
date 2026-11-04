package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/web/templates"
)

// listDeployments — history table (also used for hx-refresh).
func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	deps, err := s.q.ListDeploymentSummariesByApplication(r.Context(), c.App.ID)
	if err != nil {
		logFrom(r).Error("listDeployments: query deployments failed", "err", err, "app_id", c.App.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.DeploymentsList(c, deps))
}

// deploymentLogPage — xterm page for a specific deployment.
func (s *Server) deploymentLogPage(w http.ResponseWriter, r *http.Request) {
	c, dep, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.DeploymentLog(c, dep))
}

// deploymentLogWS — WS stream of the deployment log (live for running / saved for finished).
func (s *Server) deploymentLogWS(w http.ResponseWriter, r *http.Request) {
	_, dep, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Error("deploymentLogWS: websocket accept failed", "err", err, "deploy_id", dep.ID)
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())

	if s.logHub != nil && s.logHub.Active(dep.ID) {
		// live deployment: subscribe to the hub (buffer + tail)
		sub := s.logHub.Subscribe(dep.ID)
		defer s.logHub.Unsubscribe(dep.ID, sub)
		for {
			select {
			case <-ctx.Done():
				return
			case line, open := <-sub:
				if !open {
					conn.Close(websocket.StatusNormalClosure, "")
					return
				}
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := conn.Write(wctx, websocket.MessageText, []byte(line))
				cancel()
				if err != nil {
					logFrom(r).Info("deploymentLogWS: live log write failed", "err", err, "deploy_id", dep.ID)
					return
				}
			}
		}
	}

	// finished deployment: serve the saved log (or a cleanup message) and close
	body := dep.Log
	if body == "" {
		body = "(log cleared — retained for 1 hour)\n"
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = conn.Write(wctx, websocket.MessageText, []byte(body))
	cancel()
	conn.Close(websocket.StatusNormalClosure, "")
}

// loadDeployment loads the deployment and verifies it belongs to the application in the chain.
func (s *Server) loadDeployment(w http.ResponseWriter, r *http.Request) (templates.AppCtx, db.Deployment, bool) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return templates.AppCtx{}, db.Deployment{}, false
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "deployID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return templates.AppCtx{}, db.Deployment{}, false
	}
	dep, err := s.q.GetDeployment(r.Context(), id)
	if err != nil || dep.ApplicationID != c.App.ID {
		if err == nil && dep.ApplicationID != c.App.ID {
			logFrom(r).Info("loadDeployment: deployment does not belong to application", "deploy_id", id, "app_id", c.App.ID)
		}
		http.NotFound(w, r)
		return templates.AppCtx{}, db.Deployment{}, false
	}
	return c, dep, true
}

// rollbackApp queues a new deployment; loadDeployment checks the complete tenant chain.
func (s *Server) rollbackApp(w http.ResponseWriter, r *http.Request) {
	c, source, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	if s.deployer == nil {
		s.flashErrT(w, r, "flash.err.rollback_unavailable")
		return
	}
	id, err := s.deployer.EnqueueRollback(r.Context(), c.App.ID, source.ID)
	if err != nil {
		if errors.Is(err, deploy.ErrReleaseUnavailable) {
			s.flashErrT(w, r, "flash.err.rollback_unavailable")
		} else if errors.Is(err, deploy.ErrReleaseDrift) {
			s.flashErrT(w, r, "flash.err.rollback_drift")
		} else {
			s.flashErrT(w, r, "flash.err.rollback_refused")
		}
		return
	}
	logFrom(r).Info("application rollback queued", "app_id", c.App.ID, "source_deployment_id", source.ID, "deployment_id", id)
	s.flashOK(w, r, "flash.ok.rollback_queued")
	http.Redirect(w, r, appURL(c)+"?tab=deployments", http.StatusSeeOther)
}
