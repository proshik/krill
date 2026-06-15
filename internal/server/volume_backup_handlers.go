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

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// loadVolumeBackupChain resolves org→proj→env→app, loads {vbID}, and verifies it
// belongs (via its app_volume) to that app. Returns the app ctx for redirects.
func (s *Server) loadVolumeBackupChain(w http.ResponseWriter, r *http.Request) (templates.AppCtx, db.VolumeBackup, bool) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return templates.AppCtx{}, db.VolumeBackup{}, false
	}
	id, ok := pathID(r, "vbID")
	if !ok {
		http.NotFound(w, r)
		return templates.AppCtx{}, db.VolumeBackup{}, false
	}
	vb, err := s.q.GetVolumeBackup(r.Context(), id)
	if err != nil {
		logFrom(r).Info("loadVolumeBackupChain: backup not found", "vb_id", id, "app_id", c.App.ID)
		http.NotFound(w, r)
		return templates.AppCtx{}, db.VolumeBackup{}, false
	}
	vol, err := s.q.GetVolume(r.Context(), vb.AppVolumeID)
	if err != nil || vol.ApplicationID != c.App.ID {
		logFrom(r).Info("loadVolumeBackupChain: backup not in app", "vb_id", id, "app_id", c.App.ID)
		http.NotFound(w, r)
		return templates.AppCtx{}, db.VolumeBackup{}, false
	}
	return c, vb, true
}

// addVolumeBackup creates a backup config for an app volume (admin-only).
func (s *Server) addVolumeBackup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	v, ok := s.loadVolume(w, r, c.App.ID)
	if !ok {
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
	if err != nil || dest.OrganizationID != c.Org.ID {
		logFrom(r).Info("addVolumeBackup: destination not found or org mismatch", "destination_id", destID, "org_id", c.Org.ID)
		s.flashErr(w, r, "invalid destination")
		return
	}
	vb, err := s.q.CreateVolumeBackup(r.Context(), db.CreateVolumeBackupParams{
		AppVolumeID: v.ID, DestinationID: destID, Schedule: schedule, Prefix: prefix, Retention: int32(retention), Enabled: true,
	})
	if err != nil {
		logFrom(r).Error("addVolumeBackup: create failed", "err", err, "volume_id", v.ID)
		s.flashErr(w, r, "failed to create backup: "+err.Error())
		return
	}
	logFrom(r).Info("volume backup created", "vb_id", vb.ID, "volume_id", v.ID, "destination_id", destID, "schedule", schedule)
	if s.reloadVolumeBackups != nil {
		s.reloadVolumeBackups()
	}
	s.setFlash(w, "ok", "Backup schedule created")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// deleteVolumeBackup removes a backup config (admin-only).
func (s *Server) deleteVolumeBackup(w http.ResponseWriter, r *http.Request) {
	c, vb, ok := s.loadVolumeBackupChain(w, r)
	if !ok {
		return
	}
	if err := s.q.DeleteVolumeBackup(r.Context(), vb.ID); err != nil {
		logFrom(r).Error("deleteVolumeBackup: delete failed", "err", err, "vb_id", vb.ID)
		s.flashErr(w, r, "failed to delete backup")
		return
	}
	logFrom(r).Info("volume backup deleted", "vb_id", vb.ID)
	if s.reloadVolumeBackups != nil {
		s.reloadVolumeBackups()
	}
	s.setFlash(w, "ok", "Backup schedule deleted")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// toggleVolumeBackup flips the enabled flag of a backup config (admin-only).
func (s *Server) toggleVolumeBackup(w http.ResponseWriter, r *http.Request) {
	c, vb, ok := s.loadVolumeBackupChain(w, r)
	if !ok {
		return
	}
	if err := s.q.SetVolumeBackupEnabled(r.Context(), db.SetVolumeBackupEnabledParams{ID: vb.ID, Enabled: !vb.Enabled}); err != nil {
		logFrom(r).Error("toggleVolumeBackup: update failed", "err", err, "vb_id", vb.ID)
		s.flashErr(w, r, "failed to update backup")
		return
	}
	logFrom(r).Info("volume backup toggled", "vb_id", vb.ID, "enabled", !vb.Enabled)
	if s.reloadVolumeBackups != nil {
		s.reloadVolumeBackups()
	}
	s.setFlash(w, "ok", "Backup schedule updated")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// runVolumeBackupNow triggers an immediate backup run (admin-only). The run is
// async on a detached context; the outcome is recorded on the row's last_status.
func (s *Server) runVolumeBackupNow(w http.ResponseWriter, r *http.Request) {
	c, vb, ok := s.loadVolumeBackupChain(w, r)
	if !ok {
		return
	}
	if s.volumeSvc == nil {
		s.flashErr(w, r, "backups unavailable")
		return
	}
	logFrom(r).Info("volume backup run started", "vb_id", vb.ID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.volumeSvc.RunVolumeBackup(ctx, vb.ID, time.Now()); err != nil {
			slog.Error("runVolumeBackupNow: failed", "err", err, "vb_id", vb.ID)
		}
	}()
	s.setFlash(w, "ok", "Backup started")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// restoreVolumeBackup restores a stored object into the volume (admin-only). The
// app is quiesced (scale 0), the volume overwritten, then the app scaled back.
func (s *Server) restoreVolumeBackup(w http.ResponseWriter, r *http.Request) {
	c, vb, ok := s.loadVolumeBackupChain(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		s.flashErr(w, r, "key is required")
		return
	}
	if s.volumeSvc == nil {
		s.flashErr(w, r, "backups unavailable")
		return
	}
	logFrom(r).Info("volume backup restore started", "vb_id", vb.ID, "key", key)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.volumeSvc.RestoreByID(ctx, vb.ID, key); err != nil {
			slog.Error("restoreVolumeBackup: failed", "err", err, "vb_id", vb.ID, "key", key)
		}
	}()
	s.setFlash(w, "ok", "Restore started")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// volumeBackupObjects renders the stored objects of a backup config (HTMX partial, admin-only).
func (s *Server) volumeBackupObjects(w http.ResponseWriter, r *http.Request) {
	c, vb, ok := s.loadVolumeBackupChain(w, r)
	if !ok {
		return
	}
	if s.volumeSvc == nil {
		http.Error(w, "backups unavailable", http.StatusServiceUnavailable)
		return
	}
	objs, err := s.volumeSvc.ListObjects(r.Context(), vb.ID)
	if err != nil {
		logFrom(r).Error("volumeBackupObjects: list failed", "err", err, "vb_id", vb.ID)
		http.Error(w, "failed to list backups", http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.VolumeBackupObjects(appURL(c), vb.ID, objs))
}

// downloadVolumeBackup streams a stored backup object to the client (admin-only).
func (s *Server) downloadVolumeBackup(w http.ResponseWriter, r *http.Request) {
	_, vb, ok := s.loadVolumeBackupChain(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if s.volumeSvc == nil {
		http.Error(w, "backups unavailable", http.StatusServiceUnavailable)
		return
	}
	rc, err := s.volumeSvc.OpenObject(r.Context(), vb.ID, key)
	if err != nil {
		logFrom(r).Error("downloadVolumeBackup: open failed", "err", err, "vb_id", vb.ID, "key", key)
		http.Error(w, "failed to download backup", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+path.Base(key))
	if _, err := io.Copy(w, rc); err != nil {
		logFrom(r).Error("downloadVolumeBackup: copy failed", "err", err, "vb_id", vb.ID, "key", key)
	}
}
