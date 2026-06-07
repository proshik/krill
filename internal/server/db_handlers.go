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
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

// createDatabase creates Postgres or Redis (form: engine, name, version, external_port?).
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
		logFrom(r).Info("createDatabase: engine and name are required", "environment_id", e.ID, "engine", engine)
		s.flashErr(w, r, "engine and name are required")
		return
	}
	var extPort *int32
	if v := strings.TrimSpace(r.FormValue("external_port")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			logFrom(r).Info("createDatabase: invalid external_port", "environment_id", e.ID, "engine", engine, "name", name)
			s.flashErr(w, r, "invalid external_port")
			return
		}
		x := int32(n)
		pgN, _ := s.q.CountPostgresByExternalPort(r.Context(), &x)
		rdN, _ := s.q.CountRedisByExternalPort(r.Context(), &x)
		if pgN+rdN > 0 {
			logFrom(r).Warn("createDatabase: external port already in use", "environment_id", e.ID, "engine", engine, "name", name)
			s.flashErr(w, r, "external port already in use")
			return
		}
		extPort = &x
	}
	pw, err := genPassword()
	if err != nil {
		logFrom(r).Error("createDatabase: failed to generate password", "err", err, "environment_id", e.ID, "engine", engine, "name", name)
		s.flashErr(w, r, "internal error")
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
			DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: secret.Enc(pw),
			Image: version, ExternalPort: extPort,
		})
		if err != nil {
			logFrom(r).Error("createDatabase: failed to create postgres", "err", err, "environment_id", e.ID, "engine", engine, "name", name, "app_name", app)
			s.flashErr(w, r, "create: "+err.Error())
			return
		}
		logFrom(r).Info("database created", "environment_id", e.ID, "engine", engine, "name", name, "app_name", app)
	case "redis":
		if version == "" {
			version = "redis:7"
		}
		app := dbservice.GenerateAppName("redis", name)
		_, err := s.q.CreateRedis(r.Context(), db.CreateRedisParams{
			EnvironmentID: e.ID, Name: name, AppName: app, Password: secret.Enc(pw), Image: version, ExternalPort: extPort,
		})
		if err != nil {
			logFrom(r).Error("createDatabase: failed to create redis", "err", err, "environment_id", e.ID, "engine", engine, "name", name, "app_name", app)
			s.flashErr(w, r, "create: "+err.Error())
			return
		}
		logFrom(r).Info("database created", "environment_id", e.ID, "engine", engine, "name", name, "app_name", app)
	}
	s.setFlash(w, "ok", "Database created")
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID)+"?tab=databases", http.StatusSeeOther)
}

func (s *Server) deployDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	if eng == "postgres" {
		s.dbsvc.DeployPostgres(id)
	} else {
		s.dbsvc.DeployRedis(id)
	}
	logFrom(r).Info("database deploy requested", "db_id", id, "engine", eng)
	s.setFlash(w, "ok", "Deployment queued")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

func (s *Server) startDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	var err error
	if eng == "postgres" {
		err = s.dbsvc.StartPostgres(r.Context(), id)
	} else {
		err = s.dbsvc.StartRedis(r.Context(), id)
	}
	if err != nil {
		logFrom(r).Error("startDatabase: failed to start database", "err", err, "db_id", id, "engine", eng)
		s.flashErr(w, r, "failed to start database")
		return
	}
	logFrom(r).Info("database started", "db_id", id, "engine", eng)
	s.setFlash(w, "ok", "Start requested")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

func (s *Server) stopDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	var err error
	if eng == "postgres" {
		err = s.dbsvc.StopPostgres(r.Context(), id)
	} else {
		err = s.dbsvc.StopRedis(r.Context(), id)
	}
	if err != nil {
		logFrom(r).Error("stopDatabase: failed to stop database", "err", err, "db_id", id, "engine", eng)
		s.flashErr(w, r, "failed to stop database")
		return
	}
	logFrom(r).Info("database stopped", "db_id", id, "engine", eng)
	s.setFlash(w, "ok", "Stop requested")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
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
			logFrom(r).Error("versionDatabase: failed to update image", "err", err, "db_id", id, "engine", eng, "image", image)
			s.flashErr(w, r, "failed to update image")
			return
		}
		logFrom(r).Info("database version updated", "db_id", id, "engine", eng, "image", image)
	}
	if eng == "postgres" {
		s.dbsvc.DeployPostgres(id)
	} else {
		s.dbsvc.DeployRedis(id)
	}
	s.setFlash(w, "ok", "Version update queued")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	eng, id, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	o, _, _ := s.loadOrg(w, r)
	p, _ := s.loadProject(w, r)
	e, _ := s.loadEnvironment(w, r, p.ID)
	destroy := r.FormValue("destroy_data") == "on"
	var err error
	if eng == "postgres" {
		err = s.dbsvc.DeletePostgres(r.Context(), id, destroy)
	} else {
		err = s.dbsvc.DeleteRedis(r.Context(), id, destroy)
	}
	if err != nil {
		logFrom(r).Error("deleteDatabase: failed to delete database", "err", err, "db_id", id, "engine", eng, "destroy_data", destroy)
		s.flashErr(w, r, "failed to delete database")
		return
	}
	logFrom(r).Info("database deleted", "db_id", id, "engine", eng, "destroy_data", destroy)
	s.setFlash(w, "ok", "Database deleted")
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
		} else if err != nil {
			logFrom(r).Error("databaseStatus: engine service state failed", "err", err, "db_id", id, "engine", eng, "app_name", appName)
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
		logFrom(r).Error("databaseLogs: engine unavailable", "db_id", id, "engine", eng)
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
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Error("databaseLogs: websocket accept failed", "err", err, "db_id", id, "engine", eng, "app_name", appName)
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())
	rc, err := s.engine.ServiceLogs(ctx, appName, true)
	if err != nil {
		logFrom(r).Error("databaseLogs: engine service logs failed", "err", err, "db_id", id, "engine", eng, "app_name", appName)
		conn.Close(websocket.StatusInternalError, "logs unavailable")
		return
	}
	defer rc.Close()
	streamParsedLogsToWS(ctx, conn, rc)
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
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Error("databaseDeployLogs: websocket accept failed", "err", err, "db_id", id, "engine", eng)
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

// loadDBChain parses {engine}/{dbID} and verifies it belongs to the org→proj→env chain.
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
	// verify it belongs to the environment
	var envID int64
	if engine == "postgres" {
		row, err := s.q.GetPostgres(r.Context(), id)
		if err != nil {
			logFrom(r).Info("loadDBChain: database not found", "db_id", id, "engine", engine, "environment_id", e.ID)
			http.NotFound(w, r)
			return "", 0, false
		}
		envID = row.EnvironmentID
	} else {
		row, err := s.q.GetRedis(r.Context(), id)
		if err != nil {
			logFrom(r).Info("loadDBChain: database not found", "db_id", id, "engine", engine, "environment_id", e.ID)
			http.NotFound(w, r)
			return "", 0, false
		}
		envID = row.EnvironmentID
	}
	if envID != e.ID {
		logFrom(r).Info("loadDBChain: environment mismatch", "db_id", id, "engine", engine, "environment_id", e.ID, "db_environment_id", envID)
		http.NotFound(w, r)
		return "", 0, false
	}
	_ = o
	return engine, id, true
}

// loadDBCtx loads the chain + the DB row and assembles DatabaseCtx (connection strings).
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
		pg := dbservice.PostgresDB{AppName: row.AppName, DatabaseName: row.DatabaseName, DatabaseUser: row.DatabaseUser, DatabasePassword: secret.Dec(row.DatabasePassword), ExternalPort: row.ExternalPort}
		c.Name = row.Name
		c.Image = row.Image
		c.Status = row.Status
		c.Internal = dbservice.PostgresInternalURL(pg)
		if row.ExternalPort != nil {
			c.External = dbservice.PostgresExternalURL(pg, s.cfg.Host)
		}
		if backups, err := s.q.ListBackupsByDB(r.Context(), id); err != nil {
			logFrom(r).Error("loadDBCtx: failed to list backups", "err", err, "db_id", id)
		} else {
			c.Backups = backups
		}
		if dests, err := s.q.ListDestinationsByOrg(r.Context(), o.ID); err != nil {
			logFrom(r).Error("loadDBCtx: failed to list destinations", "err", err, "org_id", o.ID)
		} else {
			c.Destinations = dests
		}
	} else {
		row, _ := s.q.GetRedis(r.Context(), id)
		rd := dbservice.RedisDB{AppName: row.AppName, Password: secret.Dec(row.Password), ExternalPort: row.ExternalPort}
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
