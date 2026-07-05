package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// loadBackupChain resolves the org→proj→env→logical-db chain, loads {backupID}
// and verifies it belongs to that logical database. It also returns the DB
// detail base URL used for redirects and form actions.
func (s *Server) loadBackupChain(w http.ResponseWriter, r *http.Request) (db.GetBackupRow, string, bool) {
	ld, ok := s.loadLogicalDB(w, r)
	if !ok {
		return db.GetBackupRow{}, "", false
	}
	id, ok := pathID(r, "backupID")
	if !ok {
		http.NotFound(w, r)
		return db.GetBackupRow{}, "", false
	}
	b, err := s.q.GetBackup(r.Context(), id)
	if err != nil || b.LogicalDatabaseID != ld.ID {
		logFrom(r).Info("loadBackupChain: backup not found or db mismatch", "backup_id", id, "ldb_id", ld.ID)
		http.NotFound(w, r)
		return db.GetBackupRow{}, "", false
	}
	return b, s.dbBase(r, ld.ID), true
}

// dbBase builds the logical-DB detail URL (same shape logicalDatabaseDetail
// uses for templates.LogicalDBCtx.Base) directly from the chi URL params. It is
// only called after the org→proj→env→db chain has already been validated, so
// re-running the w-taking loaders (which would need a ResponseWriter to report
// errors) is unnecessary.
func (s *Server) dbBase(r *http.Request, ldbID int64) string {
	orgID, _ := pathID(r, "orgID")
	projID, _ := pathID(r, "projID")
	envID, _ := pathID(r, "envID")
	return envURL(orgID, projID, envID) + "/databases/" + strconv.FormatInt(ldbID, 10)
}

// addBackup creates a backup config for a logical database (admin-only).
func (s *Server) addBackup(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	ld, ok := s.loadLogicalDB(w, r)
	if !ok {
		return
	}
	dbID := ld.ID

	destID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("destination_id")), 10, 64)
	if err != nil {
		s.flashErrT(w, r, "flash.err.invalid_destination")
		return
	}
	schedule, serr := backup.CronForPreset(strings.TrimSpace(r.FormValue("schedule_preset")), strings.TrimSpace(r.FormValue("schedule_custom")))
	if serr != nil {
		s.flashErrT(w, r, "flash.err.invalid_schedule")
		return
	}
	retention, err := strconv.Atoi(strings.TrimSpace(r.FormValue("retention")))
	if err != nil || retention < 1 {
		s.flashErrT(w, r, "flash.err.retention_min")
		return
	}
	prefix := strings.TrimSpace(r.FormValue("prefix"))

	dest, err := s.q.GetDestination(r.Context(), destID)
	if err != nil || dest.OrganizationID != o.ID {
		logFrom(r).Info("addBackup: destination not found or org mismatch", "destination_id", destID, "org_id", o.ID, "db_id", dbID)
		s.flashErrT(w, r, "flash.err.invalid_destination")
		return
	}

	b, err := s.q.CreateBackup(r.Context(), db.CreateBackupParams{
		LogicalDatabaseID: dbID,
		DestinationID:     destID,
		Schedule:          schedule,
		Prefix:            prefix,
		Retention:         int32(retention),
		Enabled:           true,
	})
	if err != nil {
		logFrom(r).Error("addBackup: create failed", "err", err, "db_id", dbID, "destination_id", destID)
		s.flashErrErr(w, r, "flash.err.create_backup", err)
		return
	}
	logFrom(r).Info("backup created", "backup_id", b.ID, "db_id", dbID, "destination_id", destID, "schedule", schedule, "retention", retention)
	if s.reloadBackups != nil {
		s.reloadBackups()
	}
	s.flashOK(w, r, "flash.ok.backup_created")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// deleteBackup removes a backup config (admin-only).
func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	if err := s.q.DeleteBackup(r.Context(), b.ID); err != nil {
		logFrom(r).Error("deleteBackup: delete failed", "err", err, "backup_id", b.ID)
		s.flashErrT(w, r, "flash.err.delete_backup")
		return
	}
	logFrom(r).Info("backup deleted", "backup_id", b.ID, "db_id", b.LogicalDatabaseID)
	if s.reloadBackups != nil {
		s.reloadBackups()
	}
	s.flashOK(w, r, "flash.ok.backup_deleted")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// toggleBackup flips the enabled flag of a backup config (admin-only).
func (s *Server) toggleBackup(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	if err := s.q.SetBackupEnabled(r.Context(), db.SetBackupEnabledParams{ID: b.ID, Enabled: !b.Enabled}); err != nil {
		logFrom(r).Error("toggleBackup: update failed", "err", err, "backup_id", b.ID)
		s.flashErrT(w, r, "flash.err.update_backup")
		return
	}
	logFrom(r).Info("backup toggled", "backup_id", b.ID, "db_id", b.LogicalDatabaseID, "enabled", !b.Enabled)
	if s.reloadBackups != nil {
		s.reloadBackups()
	}
	s.flashOK(w, r, "flash.ok.backup_updated")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// runBackupNow triggers an immediate backup run (admin-only). The outcome is
// recorded on the row's last_status; the handler always redirects.
func (s *Server) runBackupNow(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	if s.backupSvc == nil {
		s.flashErrT(w, r, "flash.err.backups_unavailable")
		return
	}
	// Run asynchronously with a detached context: a backup can take minutes, so
	// it must not tie up the request and must finish even if the client
	// disconnects. RunBackup records the outcome on the row's last_status.
	logFrom(r).Info("backup run started", "backup_id", b.ID, "db_id", b.LogicalDatabaseID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.backupSvc.RunBackup(ctx, b.ID, time.Now()); err != nil {
			slog.Error("runBackupNow: backup failed", "err", err, "backup_id", b.ID)
		}
	}()
	s.flashOK(w, r, "flash.ok.backup_started")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// restoreBackup restores a stored object into the DB (admin-only).
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.loadBackupChain(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		s.flashErrT(w, r, "flash.err.key_required")
		return
	}
	if s.backupSvc == nil {
		s.flashErrT(w, r, "flash.err.backups_unavailable")
		return
	}
	// Run asynchronously with a detached context: the S3→psql restore can take
	// minutes, so a client disconnect must not abort a half-done restore.
	logFrom(r).Info("backup restore started", "backup_id", b.ID, "db_id", b.LogicalDatabaseID, "key", key)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.backupSvc.RestoreByID(ctx, b.ID, key); err != nil {
			slog.Error("restoreBackup: restore failed", "err", err, "backup_id", b.ID, "key", key)
		}
	}()
	s.flashOK(w, r, "flash.ok.restore_started")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// backupObjects renders the stored objects of a backup config (HTMX partial, admin-only).
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
		logFrom(r).Error("backupObjects: list failed", "err", err, "backup_id", b.ID, "db_id", b.LogicalDatabaseID)
		http.Error(w, "failed to list backups", http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.BackupObjects(base, b.ID, objs))
}

// downloadBackup streams a stored backup object to the client (admin-only).
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
		logFrom(r).Error("downloadBackup: open failed", "err", err, "backup_id", b.ID, "db_id", b.LogicalDatabaseID, "key", key)
		http.Error(w, "failed to download backup", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+path.Base(key))
	if _, err := io.Copy(w, rc); err != nil {
		logFrom(r).Error("downloadBackup: copy failed", "err", err, "backup_id", b.ID, "db_id", b.LogicalDatabaseID, "key", key)
	}
}
