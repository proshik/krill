package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/web/templates"
)

// createDatabase создаёт Postgres или Redis (form: engine, name, version, external_port?).
func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return
	}
	engine := r.FormValue("engine")
	name := strings.TrimSpace(r.FormValue("name"))
	version := strings.TrimSpace(r.FormValue("version"))
	if name == "" || (engine != "postgres" && engine != "redis") {
		http.Error(w, "engine и name обязательны", http.StatusBadRequest)
		return
	}
	var extPort *int32
	if v := strings.TrimSpace(r.FormValue("external_port")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			http.Error(w, "invalid external_port", http.StatusBadRequest)
			return
		}
		x := int32(n)
		pgN, _ := s.q.CountPostgresByExternalPort(r.Context(), &x)
		rdN, _ := s.q.CountRedisByExternalPort(r.Context(), &x)
		if pgN+rdN > 0 {
			http.Error(w, "external port already in use", http.StatusBadRequest)
			return
		}
		extPort = &x
	}
	pw, err := genPassword()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	switch engine {
	case "postgres":
		if version == "" {
			version = "postgres:17"
		}
		app := dbservice.GenerateAppName("postgres", name)
		_, err := s.q.CreatePostgres(r.Context(), db.CreatePostgresParams{
			EnvironmentID: e.ID, Name: name, AppName: app,
			DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: pw,
			Image: version, ExternalPort: extPort,
		})
		if err != nil {
			http.Error(w, "create: "+err.Error(), http.StatusBadRequest)
			return
		}
	case "redis":
		if version == "" {
			version = "redis:7"
		}
		app := dbservice.GenerateAppName("redis", name)
		_, err := s.q.CreateRedis(r.Context(), db.CreateRedisParams{
			EnvironmentID: e.ID, Name: name, AppName: app, Password: pw, Image: version, ExternalPort: extPort,
		})
		if err != nil {
			http.Error(w, "create: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID)+"?tab=databases", http.StatusSeeOther)
}

func (s *Server) deployDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	if eng == "postgres" {
		s.dbsvc.DeployPostgres(r.Context(), id)
	} else {
		s.dbsvc.DeployRedis(r.Context(), id)
	}
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

func (s *Server) startDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	if eng == "postgres" {
		_ = s.dbsvc.StartPostgres(r.Context(), id)
	} else {
		_ = s.dbsvc.StartRedis(r.Context(), id)
	}
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

func (s *Server) stopDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	if eng == "postgres" {
		_ = s.dbsvc.StopPostgres(r.Context(), id)
	} else {
		_ = s.dbsvc.StopRedis(r.Context(), id)
	}
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

func (s *Server) versionDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	image := strings.TrimSpace(r.FormValue("image"))
	if image != "" {
		var err error
		if eng == "postgres" {
			err = s.q.UpdatePostgresImage(r.Context(), db.UpdatePostgresImageParams{ID: id, Image: image})
		} else {
			err = s.q.UpdateRedisImage(r.Context(), db.UpdateRedisImageParams{ID: id, Image: image})
		}
		if err != nil {
			http.Error(w, "failed to update image", http.StatusInternalServerError)
			return
		}
	}
	if eng == "postgres" {
		s.dbsvc.DeployPostgres(r.Context(), id)
	} else {
		s.dbsvc.DeployRedis(r.Context(), id)
	}
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	o, _, _ := s.loadOrg(w, r)
	p, _ := s.loadProject(w, r)
	e, _ := s.loadEnvironment(w, r, p.ID)
	if eng == "postgres" {
		_ = s.dbsvc.DeletePostgres(r.Context(), id)
	} else {
		_ = s.dbsvc.DeleteRedis(r.Context(), id)
	}
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID)+"?tab=databases", http.StatusSeeOther)
}

func (s *Server) databaseDetail(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadDBCtx(w, r)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.DatabaseDetail(c))
}

func (s *Server) databaseStatus(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	var status, appName string
	if eng == "postgres" {
		pg, _ := s.q.GetPostgres(r.Context(), id)
		status = pg.Status
		appName = pg.AppName
	} else {
		rd, _ := s.q.GetRedis(r.Context(), id)
		status = rd.Status
		appName = rd.AppName
	}
	if s.engine != nil {
		if st, err := s.engine.ServiceState(r.Context(), appName); err == nil && st.Found {
			if st.Running >= st.Desired && st.Desired > 0 {
				status = "running"
			}
		}
	}
	render(w, r, http.StatusOK, templates.StatusBadge(status))
}

func (s *Server) databaseLogs(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	if s.engine == nil {
		http.Error(w, "engine unavailable", http.StatusServiceUnavailable)
		return
	}
	var appName string
	if eng == "postgres" {
		pg, _ := s.q.GetPostgres(r.Context(), id)
		appName = pg.AppName
	} else {
		rd, _ := s.q.GetRedis(r.Context(), id)
		appName = rd.AppName
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())
	rc, err := s.engine.ServiceLogs(ctx, appName, true)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "logs unavailable")
		return
	}
	defer rc.Close()
	streamReaderToWS(ctx, conn, rc)
}

func (s *Server) databaseDeployLogs(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	feed := dbservice.PgFeedID(id)
	if eng == "redis" {
		feed = dbservice.RedisFeedID(id)
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())
	if s.logHub != nil && s.logHub.Active(feed) {
		sub := s.logHub.Subscribe(feed)
		defer s.logHub.Unsubscribe(feed, sub)
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
				if err := conn.Write(wctx, websocket.MessageText, []byte(line)); err != nil {
					cancel()
					return
				}
				cancel()
			}
		}
	}
	conn.Write(ctx, websocket.MessageText, []byte("(no active deploy)\n"))
	conn.Close(websocket.StatusNormalClosure, "")
}

// loadDBChain парсит {engine}/{dbID} и проверяет принадлежность цепочке org→proj→env.
func (s *Server) loadDBChain(w http.ResponseWriter, r *http.Request) (string, int64, bool) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return "", 0, false
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return "", 0, false
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return "", 0, false
	}
	engine := chi.URLParam(r, "engine")
	id, err := strconv.ParseInt(chi.URLParam(r, "dbID"), 10, 64)
	if err != nil || (engine != "postgres" && engine != "redis") {
		http.NotFound(w, r)
		return "", 0, false
	}
	// проверка принадлежности окружению
	var envID int64
	if engine == "postgres" {
		row, err := s.q.GetPostgres(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return "", 0, false
		}
		envID = row.EnvironmentID
	} else {
		row, err := s.q.GetRedis(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return "", 0, false
		}
		envID = row.EnvironmentID
	}
	if envID != e.ID {
		http.NotFound(w, r)
		return "", 0, false
	}
	_ = o
	return engine, id, true
}

// loadDBCtx грузит цепочку + строку БД и собирает DatabaseCtx (connection strings).
func (s *Server) loadDBCtx(w http.ResponseWriter, r *http.Request) (templates.DatabaseCtx, bool) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return templates.DatabaseCtx{}, false
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return templates.DatabaseCtx{}, false
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return templates.DatabaseCtx{}, false
	}
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return templates.DatabaseCtx{}, false
	}
	base := envURL(o.ID, p.ID, e.ID) + "/databases/" + eng + "/" + strconv.FormatInt(id, 10)
	c := templates.DatabaseCtx{Org: o, Role: role, Project: p, Env: e, Engine: eng, ID: id, Base: base}
	if eng == "postgres" {
		row, _ := s.q.GetPostgres(r.Context(), id)
		pg := dbservice.PostgresDB{AppName: row.AppName, DatabaseName: row.DatabaseName, DatabaseUser: row.DatabaseUser, DatabasePassword: row.DatabasePassword, ExternalPort: row.ExternalPort}
		c.Name = row.Name
		c.Image = row.Image
		c.Status = row.Status
		c.Internal = dbservice.PostgresInternalURL(pg)
		if row.ExternalPort != nil {
			c.External = dbservice.PostgresExternalURL(pg, s.cfg.Host)
		}
	} else {
		row, _ := s.q.GetRedis(r.Context(), id)
		rd := dbservice.RedisDB{AppName: row.AppName, Password: row.Password, ExternalPort: row.ExternalPort}
		c.Name = row.Name
		c.Image = row.Image
		c.Status = row.Status
		c.Internal = dbservice.RedisInternalURL(rd)
		if row.ExternalPort != nil {
			c.External = dbservice.RedisExternalURL(rd, s.cfg.Host)
		}
	}
	return c, true
}

func genPassword() (string, error) {
	t, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	if len(t) < 16 {
		return "", fmt.Errorf("generated password too short")
	}
	return t[:16], nil
}
