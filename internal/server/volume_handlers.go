package server

import (
	"errors"
	"net/http"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/volume"
	"github.com/proshik/krill/internal/web/i18n"
)

// flashValidation renders a volume.ValidationError through i18n; any other error
// falls back to its raw message.
func (s *Server) flashValidation(w http.ResponseWriter, r *http.Request, err error) {
	var ve *volume.ValidationError
	if errors.As(err, &ve) {
		s.flashErr(w, r, i18n.Tf(r.Context(), ve.Key, ve.Args...))
		return
	}
	s.flashErr(w, r, err.Error())
}

// clusterHasWorkers reports whether the cluster has worker nodes (⟺ multi-node,
// since cluster_nodes holds only workers). On a count error it logs and returns
// false — the volume-owner placement guard is best-effort and fails open.
func (s *Server) clusterHasWorkers(r *http.Request) bool {
	n, err := s.q.CountClusterNodes(r.Context())
	if err != nil {
		logFrom(r).Warn("count cluster nodes failed; skipping volume-owner placement guard", "err", err)
		return false
	}
	return n > 0
}

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
		s.flashValidation(w, r, err)
		return
	}
	existing, err := s.q.ListVolumesByApplication(r.Context(), c.App.ID)
	if err != nil {
		logFrom(r).Error("addVolume: list volumes failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.add_volume")
		return
	}
	for _, v := range existing {
		if v.Name == name {
			s.flashErrT(w, r, "flash.err.volume_name_exists")
			return
		}
		if v.MountPath == mountPath {
			s.flashErrT(w, r, "flash.err.volume_path_exists")
			return
		}
	}
	_, _, normOwner, oerr := volume.ParseOwner(r.FormValue("owner"))
	if oerr != nil {
		s.flashValidation(w, r, oerr)
		return
	}
	if normOwner != "" && c.App.PlacementMode == "any" && s.clusterHasWorkers(r) {
		s.flashErrT(w, r, "flash.err.vol_owner_needs_pin")
		return
	}
	var ownerCol *string
	if normOwner != "" {
		ownerCol = &normOwner
	}
	if _, err := s.q.CreateVolume(r.Context(), db.CreateVolumeParams{
		ApplicationID: c.App.ID, Name: name, MountPath: mountPath, Owner: ownerCol,
	}); err != nil {
		logFrom(r).Error("addVolume: create failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.add_volume")
		return
	}
	logFrom(r).Info("volume added", "app_id", c.App.ID, "name", name, "mount_path", mountPath)
	s.flashOK(w, r, "flash.ok.volume_added")
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
		s.flashErrT(w, r, "flash.err.delete_volume")
		return
	}
	s.reloadVolumeBackupSchedules() // its volume backups cascade-deleted with the row
	logFrom(r).Info("volume deleted", "app_id", c.App.ID, "volume_id", v.ID, "name", v.Name)
	s.flashOK(w, r, "flash.ok.volume_removed")
	http.Redirect(w, r, appURL(c)+"?tab=volumes", http.StatusSeeOther)
}

// setVolumeOwner sets (or clears, when empty) the uid:gid the volume is chowned
// to before each deploy. Applied on the next deploy.
func (s *Server) setVolumeOwner(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	v, ok := s.loadVolume(w, r, c.App.ID)
	if !ok {
		return
	}
	_, _, normOwner, oerr := volume.ParseOwner(r.FormValue("owner"))
	if oerr != nil {
		s.flashValidation(w, r, oerr)
		return
	}
	if normOwner != "" && c.App.PlacementMode == "any" && s.clusterHasWorkers(r) {
		s.flashErrT(w, r, "flash.err.vol_owner_needs_pin")
		return
	}
	var ownerCol *string
	if normOwner != "" {
		ownerCol = &normOwner
	}
	if err := s.q.SetVolumeOwner(r.Context(), db.SetVolumeOwnerParams{ID: v.ID, Owner: ownerCol}); err != nil {
		logFrom(r).Error("setVolumeOwner: update failed", "err", err, "volume_id", v.ID)
		s.flashErrT(w, r, "flash.err.set_volume_owner")
		return
	}
	logFrom(r).Info("volume owner set", "app_id", c.App.ID, "volume_id", v.ID, "owner", normOwner)
	s.flashOK(w, r, "flash.ok.volume_owner_set")
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
