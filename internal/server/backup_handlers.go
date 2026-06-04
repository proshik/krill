package server

import (
	"context"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// loadBackupChain resolves the org→proj→env→db chain, rejects non-postgres DBs,
// loads {backupID} and verifies it belongs to that postgres DB. It also returns
// the DB detail base URL used for redirects and form actions.
func (s *Server) loadBackupChain(w http.ResponseWriter, r *http.Request) (db.Backup, string, bool) {
	engine, dbID, ok := s.loadDBChain(w, r)
	if !ok {
		return db.Backup{}, "", false
	}
	if engine != "postgres" {
		http.NotFound(w, r)
		return db.Backup{}, "", false
	}
	id, ok := pathID(r, "backupID")
	if !ok {
		http.NotFound(w, r)
		return db.Backup{}, "", false
	}
	b, err := s.q.GetBackup(r.Context(), id)
	if err != nil || b.PostgresDbID != dbID {
		logFrom(r).Info("loadBackupChain: backup not found or db mismatch", "backup_id", id, "db_id", dbID)
		http.NotFound(w, r)
		return db.Backup{}, "", false
	}
	return b, s.dbBase(r, engine, dbID), true
}

// dbBase builds the DB detail URL (same shape loadDBCtx uses for templates.Base).
func (s *Server) dbBase(r *http.Request, engine string, dbID int64) string {
	o, _, _ := s.loadOrg(nil, r)
	p, _ := s.loadProject(nil, r)
	e, _ := s.loadEnvironment(nil, r, p.ID)
	return envURL(o.ID, p.ID, e.ID) + "/databases/" + engine + "/" + strconv.FormatInt(dbID, 10)
}

// addBackup creates a backup config for a postgres DB (admin-only).
func (s *Server) addBackup(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	engine, dbID, ok := s.loadDBChain(w, r)
	if !ok {
		return
	}
	if engine != "postgres" {
		http.NotFound(w, r)
		return
	}

	destID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("destination_id")), 10, 64)
	if err != nil {
		s.flashErr(w, r, "invalid destination")
		return
	}
	schedule := strings.TrimSpace(r.FormValue("schedule"))
	if schedule == "" {
		s.flashErr(w, r, "schedule is required")
		return
	}
	retention, err := strconv.Atoi(strings.TrimSpace(r.FormValue("retention")))
	if err != nil || retention < 1 {
		s.flashErr(w, r, "retention must be at least 1")
		return
	}
	prefix := strings.TrimSpace(r.FormValue("prefix"))

	dest, err := s.q.GetDestination(r.Context(), destID)
	if err != nil || dest.OrganizationID != o.ID {
		logFrom(r).Info("addBackup: destination not found or org mismatch", "destination_id", destID, "org_id", o.ID, "db_id", dbID)
		s.flashErr(w, r, "invalid destination")
		return
	}

	b, err := s.q.CreateBackup(r.Context(), db.CreateBackupParams{
		PostgresDbID:  dbID,
		DestinationID: destID,
		Schedule:      schedule,
		Prefix:        prefix,
		Retention:     int32(retention),
		Enabled:       true,
	})
	if err != nil {
		logFrom(r).Error("addBackup: create failed", "err", err, "db_id", dbID, "destination_id", destID)
		s.flashErr(w, r, "failed to create backup: "+err.Error())
		return
	}
	logFrom(r).Info("backup created", "backup_id", b.ID, "db_id", dbID, "destination_id", destID, "schedule", schedule, "retention", retention)
	if s.reloadBackups != nil {
		s.reloadBackups()
	}
	s.setFlash(w, "ok", "Backup schedule created")
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

// deleteBackup removes a backup config (admin-only).
func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	if err := s.q.DeleteBackup(r.Context(), b.ID); err != nil {
		logFrom(r).Error("deleteBackup: delete failed", "err", err, "backup_id", b.ID)
		s.flashErr(w, r, "failed to delete backup")
		return
	}
	logFrom(r).Info("backup deleted", "backup_id", b.ID, "db_id", b.PostgresDbID)
	if s.reloadBackups != nil {
		s.reloadBackups()
	}
	s.setFlash(w, "ok", "Backup schedule deleted")
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

// toggleBackup flips the enabled flag of a backup config (admin-only).
func (s *Server) toggleBackup(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	if err := s.q.SetBackupEnabled(r.Context(), db.SetBackupEnabledParams{ID: b.ID, Enabled: !b.Enabled}); err != nil {
		logFrom(r).Error("toggleBackup: update failed", "err", err, "backup_id", b.ID)
		s.flashErr(w, r, "failed to update backup")
		return
	}
	logFrom(r).Info("backup toggled", "backup_id", b.ID, "db_id", b.PostgresDbID, "enabled", !b.Enabled)
	if s.reloadBackups != nil {
		s.reloadBackups()
	}
	s.setFlash(w, "ok", "Backup schedule updated")
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

// runBackupNow triggers an immediate backup run (admin-only). The outcome is
// recorded on the row's last_status; the handler always redirects.
func (s *Server) runBackupNow(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	if s.backupSvc == nil {
		http.Error(w, "backups unavailable", http.StatusServiceUnavailable)
		return
	}
	// Detached context: a manual backup should finish even if the client disconnects.
	if err := s.backupSvc.RunBackup(context.Background(), b.ID, time.Now()); err != nil {
		logFrom(r).Error("runBackupNow: backup run failed", "err", err, "backup_id", b.ID, "db_id", b.PostgresDbID)
	} else {
		logFrom(r).Info("backup run completed", "backup_id", b.ID, "db_id", b.PostgresDbID)
	}
	s.setFlash(w, "ok", "Backup started")
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

// restoreBackup restores a stored object into the DB (admin-only).
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if s.backupSvc == nil {
		http.Error(w, "backups unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.backupSvc.RestoreByID(r.Context(), b.ID, key); err != nil {
		logFrom(r).Error("restoreBackup: restore failed", "err", err, "backup_id", b.ID, "db_id", b.PostgresDbID, "key", key)
		s.flashErr(w, r, "failed to restore backup")
		return
	}
	logFrom(r).Info("backup restored", "backup_id", b.ID, "db_id", b.PostgresDbID, "key", key)
	s.setFlash(w, "ok", "Restore started")
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

// backupObjects renders the stored objects of a backup config (HTMX partial, member-readable).
func (s *Server) backupObjects(w http.ResponseWriter, r *http.Request) {
	b, base, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	if s.backupSvc == nil {
		http.Error(w, "backups unavailable", http.StatusServiceUnavailable)
		return
	}
	objs, err := s.backupSvc.ListObjects(r.Context(), b.ID)
	if err != nil {
		logFrom(r).Error("backupObjects: list failed", "err", err, "backup_id", b.ID, "db_id", b.PostgresDbID)
		http.Error(w, "failed to list backups", http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.BackupObjects(base, b.ID, objs))
}

// downloadBackup streams a stored backup object to the client (member-readable).
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if s.backupSvc == nil {
		http.Error(w, "backups unavailable", http.StatusServiceUnavailable)
		return
	}
	rc, err := s.backupSvc.OpenObject(r.Context(), b.ID, key)
	if err != nil {
		logFrom(r).Error("downloadBackup: open failed", "err", err, "backup_id", b.ID, "db_id", b.PostgresDbID, "key", key)
		http.Error(w, "failed to download backup", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+path.Base(key))
	if _, err := io.Copy(w, rc); err != nil {
		logFrom(r).Error("downloadBackup: copy failed", "err", err, "backup_id", b.ID, "db_id", b.PostgresDbID, "key", key)
	}
}
