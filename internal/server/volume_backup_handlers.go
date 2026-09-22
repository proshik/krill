package server

import (
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/backup"
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
	if err != nil || dest.OrganizationID != c.Org.ID {
		logFrom(r).Info("addVolumeBackup: destination not found or org mismatch", "destination_id", destID, "org_id", c.Org.ID)
		s.flashErrT(w, r, "flash.err.invalid_destination")
		return
	}
	// Same destination and prefix = the same S3 directory, where each config's
	// retention would delete the other's archives (see addBackup).
	s.createMu.Lock()
	defer s.createMu.Unlock()
	existing, err := s.q.ListVolumeBackupsByVolume(r.Context(), v.ID)
	if err != nil {
		logFrom(r).Error("addVolumeBackup: list backups failed", "err", err, "volume_id", v.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	for _, e := range existing {
		if e.DestinationID == destID && e.Prefix == prefix {
			s.flashErrT(w, r, "flash.err.backup_duplicate")
			return
		}
	}
	vb, err := s.q.CreateVolumeBackup(r.Context(), db.CreateVolumeBackupParams{
		AppVolumeID: v.ID, DestinationID: destID, Schedule: schedule, Prefix: prefix, Retention: int32(retention), Enabled: true,
	})
	if err != nil {
		logFrom(r).Error("addVolumeBackup: create failed", "err", err, "volume_id", v.ID)
		s.flashErrErr(w, r, "flash.err.create_backup", err)
		return
	}
	logFrom(r).Info("volume backup created", "vb_id", vb.ID, "volume_id", v.ID, "destination_id", destID, "schedule", schedule)
	if s.reloadVolumeBackups != nil {
		s.reloadVolumeBackups()
	}
	s.flashOK(w, r, "flash.ok.backup_created")
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
		s.flashErrT(w, r, "flash.err.delete_backup")
		return
	}
	logFrom(r).Info("volume backup deleted", "vb_id", vb.ID)
	if s.reloadVolumeBackups != nil {
		s.reloadVolumeBackups()
	}
	s.flashOK(w, r, "flash.ok.backup_deleted")
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
		s.flashErrT(w, r, "flash.err.update_backup")
		return
	}
	logFrom(r).Info("volume backup toggled", "vb_id", vb.ID, "enabled", !vb.Enabled)
	if s.reloadVolumeBackups != nil {
		s.reloadVolumeBackups()
	}
	s.flashOK(w, r, "flash.ok.backup_updated")
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
		s.flashErrT(w, r, "flash.err.backups_unavailable")
		return
	}
	if err := s.volumeSvc.StartVolumeBackup(vb.ID, time.Now(), 30*time.Minute); err != nil {
		logFrom(r).Info("volume backup run refused", "err", err, "vb_id", vb.ID)
		s.flashErrT(w, r, "flash.err.backup_running")
		return
	}
	logFrom(r).Info("volume backup run started", "vb_id", vb.ID)
	s.flashOK(w, r, "flash.ok.backup_started")
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
		s.flashErrT(w, r, "flash.err.key_required")
		return
	}
	if s.volumeSvc == nil {
		s.flashErrT(w, r, "flash.err.backups_unavailable")
		return
	}
	if err := s.volumeSvc.StartRestore(vb.ID, key, 30*time.Minute); err != nil {
		logFrom(r).Info("volume backup restore refused", "err", err, "vb_id", vb.ID)
		s.flashErrT(w, r, "flash.err.backup_running")
		return
	}
	logFrom(r).Info("volume backup restore started", "vb_id", vb.ID, "key", key)
	s.flashOK(w, r, "flash.ok.restore_started")
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
