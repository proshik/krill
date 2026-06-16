package server

import (
	"net/http"
	"strconv"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

// listDestinations renders the org's S3 destinations (readable by any member).
func (s *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	dests, err := s.q.ListDestinationsByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("listDestinations: failed to list destinations", "err", err, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.Destinations(o, role, dests))
}

// createDestination validates the form, checks bucket access, and persists a
// new destination (admin-only). Credentials are never logged.
func (s *Server) createDestination(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	name := r.FormValue("name")
	endpoint := r.FormValue("endpoint")
	bucket := r.FormValue("bucket")
	region := r.FormValue("region")
	accessKey := r.FormValue("access_key")
	secretKey := r.FormValue("secret_key")

	if name == "" || bucket == "" {
		s.flashErrT(w, r, "flash.err.dest_fields_required")
		return
	}
	if region == "" {
		region = "us-east-1"
	}

	n, err := s.q.CountDestinationsByName(r.Context(), db.CountDestinationsByNameParams{OrganizationID: o.ID, Name: name})
	if err != nil {
		logFrom(r).Error("createDestination: failed to count destinations", "err", err, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	if n > 0 {
		s.flashErrT(w, r, "flash.err.dest_name_exists")
		return
	}

	if err := backup.CheckAccess(r.Context(), backup.Destination{
		Endpoint:  endpoint,
		Bucket:    bucket,
		Region:    region,
		AccessKey: accessKey,
		SecretKey: secretKey,
	}); err != nil {
		logFrom(r).Info("createDestination: bucket access check failed", "err", err, "org_id", o.ID, "bucket", bucket, "endpoint", endpoint)
		s.flashErrT(w, r, "flash.err.bucket_access")
		return
	}

	d, err := s.q.CreateDestination(r.Context(), db.CreateDestinationParams{
		OrganizationID: o.ID,
		Name:           name,
		Endpoint:       endpoint,
		Bucket:         bucket,
		Region:         region,
		AccessKey:      secret.Enc(accessKey),
		SecretKey:      secret.Enc(secretKey),
	})
	if err != nil {
		logFrom(r).Error("createDestination: failed to create destination", "err", err, "org_id", o.ID, "name", name)
		s.flashErrErr(w, r, "flash.err.create_destination", err)
		return
	}
	logFrom(r).Info("destination created", "org_id", o.ID, "destination_id", d.ID, "name", d.Name, "bucket", d.Bucket, "endpoint", d.Endpoint)
	s.flashOK(w, r, "flash.ok.dest_created")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/destinations", http.StatusSeeOther)
}

// deleteDestination removes a destination (admin-only). Deletion is blocked
// while backups still reference it.
func (s *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	dID, ok := pathID(r, "destID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := s.q.GetDestination(r.Context(), dID)
	if err != nil || d.OrganizationID != o.ID {
		logFrom(r).Info("deleteDestination: destination not found or org mismatch", "destination_id", dID, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	n, err := s.q.CountBackupsByDestination(r.Context(), dID)
	if err != nil {
		logFrom(r).Error("deleteDestination: failed to count backups", "err", err, "destination_id", dID, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	if n > 0 {
		s.flashErrT(w, r, "flash.err.dest_in_use")
		return
	}
	if err := s.q.DeleteDestination(r.Context(), dID); err != nil {
		logFrom(r).Error("deleteDestination: failed to delete destination", "err", err, "destination_id", dID, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("destination deleted", "org_id", o.ID, "destination_id", dID)
	s.flashOK(w, r, "flash.ok.dest_deleted")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/destinations", http.StatusSeeOther)
}
