package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/topology"
)

// envView builds the environment's systems view from rows the caller already
// loaded (apps with live statuses, logical databases) plus the environment's
// systems, buckets and links. Everything it reads is scoped to this
// environment or to servers its own apps link to, so nothing of another
// environment reaches the page beyond a server's name and a "shared" flag.
func (s *Server) envView(ctx context.Context, orgID, projID int64, env db.Environment, apps []db.Application, ldbs []db.ListLogicalDatabasesByEnvironmentRow, buckets []db.ListS3BucketsByEnvironmentRow) topology.EnvView {
	base := envURL(orgID, projID, env.ID)
	in := topology.EnvViewInput{Members: map[int64]int64{}, Resources: map[string]topology.EnvResource{}, HostedByInstance: map[int64]int{}}
	if systems, err := s.q.ListSystemsByEnvironment(ctx, env.ID); err == nil {
		for _, sy := range systems {
			in.Systems = append(in.Systems, topology.EnvSystem{ID: sy.ID, Name: sy.Name})
		}
	} else {
		slog.Warn("envView: list systems", "err", err)
	}
	if members, err := s.q.ListSystemMembersByEnvironment(ctx, env.ID); err == nil {
		for _, m := range members {
			in.Members[m.ApplicationID] = m.SystemID
		}
	} else {
		slog.Warn("envView: list members", "err", err)
	}
	appIDs := make([]int64, 0, len(apps))
	for _, a := range apps {
		in.Apps = append(in.Apps, topology.EnvApp{ID: a.ID, Name: a.Name, Image: a.Image + ":" + a.Tag, Status: a.Status})
		appIDs = append(appIDs, a.ID)
	}
	var envLogicals []topology.EnvLogical
	for _, l := range ldbs {
		key := "ldb:" + strconv.FormatInt(l.ID, 10)
		in.Resources[key] = topology.EnvResource{Key: key, Kind: "ldb", Label: l.DbName, Server: l.InstanceName, Engine: "postgres", InstanceID: l.InstanceID,
			Href: base + "/databases/" + strconv.FormatInt(l.ID, 10)}
		envLogicals = append(envLogicals, topology.EnvLogical{ID: l.ID, InstanceAppName: l.InstanceAppName, DbName: l.DbName})
	}
	for _, b := range buckets {
		key := "bucket:" + strconv.FormatInt(b.ID, 10)
		in.Resources[key] = topology.EnvResource{Key: key, Kind: "bucket", Label: b.Name, Server: b.InstanceName, Engine: "minio", InstanceID: b.InstanceID,
			Href: base + "/buckets/" + strconv.FormatInt(b.ID, 10)}
	}
	insts := map[int64]db.DbInstance{}
	var envInstances []topology.EnvInstance
	if all, err := s.q.ListDBInstancesByOrg(ctx, orgID); err == nil {
		for _, inst := range all {
			insts[inst.ID] = inst
			envInstances = append(envInstances, topology.EnvInstance{ServiceID: "db-" + strconv.FormatInt(inst.ID, 10), AppName: inst.AppName, Engine: inst.Engine})
		}
	} else {
		slog.Warn("envView: list instances", "err", err)
	}
	addInstance := func(id int64) string {
		key := "inst:" + strconv.FormatInt(id, 10)
		if _, ok := in.Resources[key]; ok {
			return key
		}
		inst, ok := insts[id]
		if !ok {
			return ""
		}
		in.Resources[key] = topology.EnvResource{Key: key, Kind: "instance", Label: inst.Name, Server: inst.Name, Engine: inst.Engine, InstanceID: inst.ID,
			Href: "/orgs/" + strconv.FormatInt(orgID, 10) + "/db-servers/" + strconv.FormatInt(inst.ID, 10)}
		return key
	}
	if len(appIDs) > 0 {
		if links, err := s.q.ListDBLinksByApplicationIDs(ctx, appIDs); err == nil {
			for _, l := range links {
				switch {
				case l.LogicalDatabaseID != nil:
					in.Links = append(in.Links, topology.EnvLink{AppID: l.ApplicationID, ResKey: "ldb:" + strconv.FormatInt(*l.LogicalDatabaseID, 10), VarName: l.VarName})
				case l.InstanceID != nil:
					if key := addInstance(*l.InstanceID); key != "" {
						in.Links = append(in.Links, topology.EnvLink{AppID: l.ApplicationID, ResKey: key, VarName: l.VarName})
					}
				}
			}
		} else {
			slog.Warn("envView: list db links", "err", err)
		}
		if links, err := s.q.ListBucketLinksByApplicationIDs(ctx, appIDs); err == nil {
			for _, l := range links {
				in.Links = append(in.Links, topology.EnvLink{AppID: l.ApplicationID, ResKey: "bucket:" + strconv.FormatInt(l.BucketID, 10), VarName: l.VarName})
			}
		} else {
			slog.Warn("envView: list bucket links", "err", err)
		}
	}
	// Connections written by hand into env values, marked as detected.
	for _, a := range apps {
		env, _ := parseEnv(a.EnvText)
		for _, d := range topology.DetectEnvLinks(env, envInstances, envLogicals) {
			key := ""
			switch d.ToKind {
			case "logical":
				key = "ldb:" + d.ToID
			case "instance":
				if id, err := strconv.ParseInt(strings.TrimPrefix(d.ToID, "db-"), 10, 64); err == nil {
					key = addInstance(id)
				}
			}
			if key != "" {
				in.Links = append(in.Links, topology.EnvLink{AppID: a.ID, ResKey: key, VarName: d.VarName, Detected: true})
			}
		}
	}
	seen := map[int64]bool{}
	for _, r := range in.Resources {
		if seen[r.InstanceID] {
			continue
		}
		seen[r.InstanceID] = true
		if n, err := s.q.CountResourcesByInstance(ctx, r.InstanceID); err == nil {
			in.HostedByInstance[r.InstanceID] = int(n)
		} else {
			slog.Warn("envView: count server resources", "err", err)
		}
	}
	return topology.BuildEnvView(in)
}

// loadSystem parses {systemID} and verifies it belongs to the environment.
func (s *Server) loadSystem(w http.ResponseWriter, r *http.Request, envID int64) (db.System, bool) {
	id, ok := pathID(r, "systemID")
	if !ok {
		http.NotFound(w, r)
		return db.System{}, false
	}
	sy, err := s.q.GetSystem(r.Context(), id)
	if err != nil || sy.EnvironmentID != envID {
		logFrom(r).Info("loadSystem: not found in environment", "system_id", id, "environment_id", envID)
		http.NotFound(w, r)
		return db.System{}, false
	}
	return sy, true
}

// systemEnv loads the org→project→environment chain for a system route.
func (s *Server) systemEnv(w http.ResponseWriter, r *http.Request) (db.Organization, db.Project, db.Environment, bool) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return o, db.Project{}, db.Environment{}, false
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return o, p, db.Environment{}, false
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	return o, p, e, ok
}

func systemName(r *http.Request) string {
	return strings.TrimSpace(r.FormValue("name"))
}

// createSystem adds an empty system to the environment. Admin-only.
func (s *Server) createSystem(w http.ResponseWriter, r *http.Request) {
	o, p, e, ok := s.systemEnv(w, r)
	if !ok {
		return
	}
	name := systemName(r)
	if name == "" || len(name) > 80 {
		s.flashErrT(w, r, "flash.err.system_name")
		return
	}
	sy, err := s.q.CreateSystem(r.Context(), db.CreateSystemParams{EnvironmentID: e.ID, Name: name})
	if err != nil {
		logFrom(r).Info("createSystem: insert refused", "err", err, "environment_id", e.ID)
		s.flashErrT(w, r, "flash.err.system_exists")
		return
	}
	logFrom(r).Info("system created", "system_id", sy.ID, "environment_id", e.ID)
	s.flashOK(w, r, "flash.ok.system_created")
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}

// renameSystem renames a system. Admin-only.
func (s *Server) renameSystem(w http.ResponseWriter, r *http.Request) {
	o, p, e, ok := s.systemEnv(w, r)
	if !ok {
		return
	}
	sy, ok := s.loadSystem(w, r, e.ID)
	if !ok {
		return
	}
	name := systemName(r)
	if name == "" || len(name) > 80 {
		s.flashErrT(w, r, "flash.err.system_name")
		return
	}
	if err := s.q.RenameSystem(r.Context(), db.RenameSystemParams{ID: sy.ID, Name: name}); err != nil {
		logFrom(r).Info("renameSystem: refused", "err", err, "system_id", sy.ID)
		s.flashErrT(w, r, "flash.err.system_exists")
		return
	}
	logFrom(r).Info("system renamed", "system_id", sy.ID)
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}

// deleteSystem removes the grouping only: its apps become ungrouped, and no
// app, database, bucket or server is touched. Admin-only.
func (s *Server) deleteSystem(w http.ResponseWriter, r *http.Request) {
	o, p, e, ok := s.systemEnv(w, r)
	if !ok {
		return
	}
	sy, ok := s.loadSystem(w, r, e.ID)
	if !ok {
		return
	}
	if err := s.q.DeleteSystem(r.Context(), sy.ID); err != nil {
		logFrom(r).Error("deleteSystem: delete failed", "err", err, "system_id", sy.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("system deleted", "system_id", sy.ID, "environment_id", e.ID)
	s.flashOK(w, r, "flash.ok.system_deleted")
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}

// addSystemApp moves an app of the environment into the system (out of any
// other one). Admin-only; changes no deployment.
func (s *Server) addSystemApp(w http.ResponseWriter, r *http.Request) {
	o, p, e, ok := s.systemEnv(w, r)
	if !ok {
		return
	}
	sy, ok := s.loadSystem(w, r, e.ID)
	if !ok {
		return
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("app_id")), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	app, err := s.q.GetApplication(r.Context(), appID)
	if err != nil || app.EnvironmentID != e.ID {
		logFrom(r).Info("addSystemApp: app not in environment", "app_id", appID, "environment_id", e.ID)
		http.NotFound(w, r)
		return
	}
	if err := s.q.SetAppSystem(r.Context(), db.SetAppSystemParams{ApplicationID: app.ID, SystemID: sy.ID}); err != nil {
		logFrom(r).Error("addSystemApp: save failed", "err", err, "app_id", app.ID, "system_id", sy.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("app added to system", "app_id", app.ID, "system_id", sy.ID)
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}

// removeSystemApp makes an app of the system ungrouped. Admin-only.
func (s *Server) removeSystemApp(w http.ResponseWriter, r *http.Request) {
	o, p, e, ok := s.systemEnv(w, r)
	if !ok {
		return
	}
	sy, ok := s.loadSystem(w, r, e.ID)
	if !ok {
		return
	}
	appID, ok := pathID(r, "appID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	members, err := s.q.ListSystemMembersByEnvironment(r.Context(), e.ID)
	if err != nil {
		logFrom(r).Error("removeSystemApp: list members", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	member := false
	for _, m := range members {
		member = member || (m.ApplicationID == appID && m.SystemID == sy.ID)
	}
	if !member {
		http.NotFound(w, r)
		return
	}
	if err := s.q.RemoveAppFromSystem(r.Context(), appID); err != nil {
		logFrom(r).Error("removeSystemApp: delete failed", "err", err, "app_id", appID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("app removed from system", "app_id", appID, "system_id", sy.ID)
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}
