package server

import (
	"net/http"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/volume"
)

// addVolume creates a named volume for the app. The mount is applied on the next
// deploy (the ServiceSpec is rebuilt from app_volumes).
func (s *Server) addVolume(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	mountPath := strings.TrimSpace(r.FormValue("mount_path"))
	if err := volume.ValidateAppVolume(name, mountPath); err != nil {
		s.flashErr(w, r, err.Error())
		return
	}
	existing, err := s.q.ListVolumesByApplication(r.Context(), c.App.ID)
	if err != nil {
		logFrom(r).Error("addVolume: list volumes failed", "err", err, "app_id", c.App.ID)
		s.flashErr(w, r, "failed to add volume")
		return
	}
	for _, v := range existing {
		if v.Name == name {
			s.flashErr(w, r, "a volume with this name already exists")
			return
		}
		if v.MountPath == mountPath {
			s.flashErr(w, r, "a volume is already mounted at this path")
			return
		}
	}
	if _, err := s.q.CreateVolume(r.Context(), db.CreateVolumeParams{
		ApplicationID: c.App.ID, Name: name, MountPath: mountPath,
	}); err != nil {
		logFrom(r).Error("addVolume: create failed", "err", err, "app_id", c.App.ID)
		s.flashErr(w, r, "failed to add volume")
		return
	}
	logFrom(r).Info("volume added", "app_id", c.App.ID, "name", name, "mount_path", mountPath)
	s.setFlash(w, "ok", "Volume added — applied on next deploy")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// deleteVolume removes the volume's model row. The Docker volume and its data are
// kept (the running service still mounts it until the next deploy); reclaim disk
// via app delete with "Destroy data", or manually.
func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	v, ok := s.loadVolume(w, r, c.App.ID)
	if !ok {
		return
	}
	if err := s.q.DeleteVolume(r.Context(), v.ID); err != nil {
		logFrom(r).Error("deleteVolume: delete failed", "err", err, "volume_id", v.ID)
		s.flashErr(w, r, "failed to delete volume")
		return
	}
	logFrom(r).Info("volume deleted", "app_id", c.App.ID, "volume_id", v.ID, "name", v.Name)
	s.setFlash(w, "ok", "Volume removed — applied on next deploy")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// loadVolume parses {volID} and verifies it belongs to the given application
// (tenancy is already checked by loadAppCtx). 404 on any mismatch.
func (s *Server) loadVolume(w http.ResponseWriter, r *http.Request, appID int64) (db.AppVolume, bool) {
	id, ok := pathID(r, "volID")
	if !ok {
		http.NotFound(w, r)
		return db.AppVolume{}, false
	}
	v, err := s.q.GetVolume(r.Context(), id)
	if err != nil || v.ApplicationID != appID {
		logFrom(r).Info("loadVolume: not found in app", "volume_id", id, "app_id", appID)
		http.NotFound(w, r)
		return db.AppVolume{}, false
	}
	return v, true
}
