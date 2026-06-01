package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// listDeployments — таблица истории (используется и для hx-refresh).
func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	deps, err := s.q.ListDeploymentsByApplication(r.Context(), c.App.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.DeploymentsList(c, deps))
}

// deploymentLogPage — страница с xterm для конкретного деплоя.
func (s *Server) deploymentLogPage(w http.ResponseWriter, r *http.Request) {
	c, dep, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.DeploymentLog(c, dep))
}

// deploymentLogWS — WS-стрим лога деплоя (live для running / сохранённый для завершённого).
func (s *Server) deploymentLogWS(w http.ResponseWriter, r *http.Request) {
	_, dep, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())

	if s.logHub != nil && s.logHub.Active(dep.ID) {
		// живой деплой: подписка на хаб (буфер + хвост)
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
					return
				}
			}
		}
	}

	// завершённый деплой: отдать сохранённый лог (или сообщение об очистке) и закрыть
	body := dep.Log
	if body == "" {
		body = "(лог очищен — хранится 1 час)\n"
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = conn.Write(wctx, websocket.MessageText, []byte(body))
	cancel()
	conn.Close(websocket.StatusNormalClosure, "")
}

// loadDeployment грузит деплой и проверяет принадлежность приложению из цепочки.
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
		http.NotFound(w, r)
		return templates.AppCtx{}, db.Deployment{}, false
	}
	return c, dep, true
}
