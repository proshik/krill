package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/bucketbackup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/web/templates"
)

// bucketBackupTimeout bounds one backup or restore run.
const bucketBackupTimeout = 2 * time.Hour

// SetBucketBackups wires the bucket-backup service and its scheduler reload.
func (s *Server) SetBucketBackups(svc *bucketbackup.Service, reload func()) {
	s.bucketBackups = svc
	s.reloadBucketBackups = reload
}

func (s *Server) reloadBucketBackupSchedules() {
	if s.reloadBucketBackups != nil {
		s.reloadBucketBackups()
	}
}

// bucketBackupViews loads a bucket's backup configurations with their
// snapshots, for the bucket page (admins only).
func (s *Server) bucketBackupViews(r *http.Request, bucketID int64) []templates.BucketBackupView {
	rows, err := s.q.ListBucketBackupsByBucket(r.Context(), bucketID)
	if err != nil {
		logFrom(r).Error("bucket page: list backups", "err", err, "bucket_id", bucketID)
		return nil
	}
	out := make([]templates.BucketBackupView, 0, len(rows))
	for _, b := range rows {
		v := templates.BucketBackupView{Row: b}
		if d, err := s.q.GetDestination(r.Context(), b.DestinationID); err == nil {
			v.Destination = d.Name
		}
		if s.bucketBackups != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			snaps, err := s.bucketBackups.Snapshots(ctx, b.ID)
			cancel()
			if err != nil {
				logFrom(r).Warn("bucket page: list snapshots", "err", err, "bucket_backup_id", b.ID)
				v.SnapshotsUnavailable = true
			}
			v.Snapshots = snaps
		}
		out = append(out, v)
	}
	return out
}

// loadBucketBackup loads {bbID} of the bucket in c (404 on mismatch).
func (s *Server) loadBucketBackup(w http.ResponseWriter, r *http.Request, c bucketCtx) (db.BucketBackup, bool) {
	id, ok := pathID(r, "bbID")
	if !ok {
		http.NotFound(w, r)
		return db.BucketBackup{}, false
	}
	b, err := s.q.GetBucketBackup(r.Context(), id)
	if err != nil || b.BucketID != c.Bucket.ID {
		logFrom(r).Info("loadBucketBackup: not found in bucket", "bucket_backup_id", id, "bucket_id", c.Bucket.ID)
		http.NotFound(w, r)
		return db.BucketBackup{}, false
	}
	return b, true
}

// addBucketBackup creates a backup configuration for the bucket. Admin-only.
func (s *Server) addBucketBackup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
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
	prefix := strings.Trim(strings.TrimSpace(r.FormValue("prefix")), "/")
	dest, err := s.q.GetDestination(r.Context(), destID)
	if err != nil || dest.OrganizationID != c.Org.ID {
		logFrom(r).Info("addBucketBackup: destination not in org", "destination_id", destID, "org_id", c.Org.ID)
		s.flashErrT(w, r, "flash.err.invalid_destination")
		return
	}
	// Same storage and prefix = one directory, where each configuration's
	// retention would delete the other's snapshots.
	s.createMu.Lock()
	defer s.createMu.Unlock()
	existing, err := s.q.ListBucketBackupsByBucket(r.Context(), c.Bucket.ID)
	if err != nil {
		logFrom(r).Error("addBucketBackup: list", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	// Compare where the snapshots land, not the destination's id: two
	// destinations of the org can name the same endpoint and bucket.
	for _, e := range existing {
		other, err := s.q.GetDestination(r.Context(), e.DestinationID)
		if err != nil {
			logFrom(r).Error("addBucketBackup: read destination", "err", err, "destination_id", e.DestinationID)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		if sameStorage(other, dest) && e.Prefix == prefix {
			s.flashErrT(w, r, "flash.err.backup_duplicate")
			return
		}
	}
	b, err := s.q.CreateBucketBackup(r.Context(), db.CreateBucketBackupParams{
		BucketID: c.Bucket.ID, DestinationID: destID, Schedule: schedule, Prefix: prefix, Retention: int32(retention),
	})
	if err != nil {
		logFrom(r).Error("addBucketBackup: create", "err", err)
		s.flashErrErr(w, r, "flash.err.create_backup", err)
		return
	}
	logFrom(r).Info("bucket backup created", "bucket_backup_id", b.ID, "bucket_id", c.Bucket.ID, "destination_id", destID, "schedule", schedule)
	s.reloadBucketBackupSchedules()
	s.flashOK(w, r, "flash.ok.backup_created")
	http.Redirect(w, r, c.url(), http.StatusSeeOther)
}

// toggleBucketBackup pauses or resumes the schedule. Admin-only.
func (s *Server) toggleBucketBackup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	b, ok := s.loadBucketBackup(w, r, c)
	if !ok {
		return
	}
	enabled, ok := s.desiredState(w, r, "enabled")
	if !ok {
		return
	}
	if err := s.q.SetBucketBackupEnabled(r.Context(), db.SetBucketBackupEnabledParams{ID: b.ID, Enabled: enabled}); err != nil {
		logFrom(r).Error("toggleBucketBackup", "err", err)
		s.flashErrT(w, r, "flash.err.update_backup")
		return
	}
	s.reloadBucketBackupSchedules()
	s.flashOK(w, r, "flash.ok.backup_updated")
	http.Redirect(w, r, c.url(), http.StatusSeeOther)
}

// deleteBucketBackup removes a configuration; its snapshots stay in storage.
// Admin-only.
func (s *Server) deleteBucketBackup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	b, ok := s.loadBucketBackup(w, r, c)
	if !ok {
		return
	}
	if err := s.q.DeleteBucketBackup(r.Context(), b.ID); err != nil {
		logFrom(r).Error("deleteBucketBackup", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("bucket backup deleted", "bucket_backup_id", b.ID)
	s.reloadBucketBackupSchedules()
	s.flashOK(w, r, "flash.ok.backup_deleted")
	http.Redirect(w, r, c.url(), http.StatusSeeOther)
}

// runBucketBackupNow starts a snapshot now. Admin-only.
func (s *Server) runBucketBackupNow(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	b, ok := s.loadBucketBackup(w, r, c)
	if !ok {
		return
	}
	if s.bucketBackups == nil {
		s.flashErrT(w, r, "flash.err.backups_unavailable")
		return
	}
	if err := s.bucketBackups.StartBackup(b.ID, time.Now(), bucketBackupTimeout); err != nil {
		logFrom(r).Info("bucket backup refused", "err", err, "bucket_backup_id", b.ID)
		s.flashErrT(w, r, "flash.err.backup_running")
		return
	}
	logFrom(r).Info("bucket backup started", "bucket_backup_id", b.ID)
	s.flashOK(w, r, "flash.ok.backup_started")
	http.Redirect(w, r, c.url(), http.StatusSeeOther)
}

// restoreBucketBackup creates a new bucket on the same server and environment
// and restores a snapshot into it. The source bucket is never written to;
// switching apps to the restored bucket is the operator's own step.
// Admin-only.
func (s *Server) restoreBucketBackup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	b, ok := s.loadBucketBackup(w, r, c)
	if !ok {
		return
	}
	if s.bucketBackups == nil {
		s.flashErrT(w, r, "flash.err.backups_unavailable")
		return
	}
	name := strings.TrimSpace(r.FormValue("target"))
	snapshot := strings.TrimSpace(r.FormValue("snapshot"))
	if name == c.Bucket.Name {
		s.flashErrT(w, r, "flash.err.restore_new_bucket")
		return
	}
	// Check the snapshot before creating anything, so a refused restore does
	// not leave an empty bucket behind.
	snaps, err := s.bucketBackups.Snapshots(r.Context(), b.ID)
	if err != nil {
		logFrom(r).Warn("restoreBucketBackup: list snapshots", "err", err, "bucket_backup_id", b.ID)
		s.flashErrErr(w, r, "flash.err.restore_failed", err)
		return
	}
	complete := false
	for _, sn := range snaps {
		complete = complete || (sn.Timestamp == snapshot && sn.Complete)
	}
	if !complete {
		s.flashErrT(w, r, "flash.err.restore_no_snapshot")
		return
	}
	di, key := s.s3Instance(r, c.Org.ID, c.Bucket.InstanceID)
	if key != "" {
		s.flashErrT(w, r, key)
		return
	}
	target, err := s.newKrillBucket(r, di, c.Env.ID, name)
	if errors.Is(err, errBucketTaken) {
		s.flashErrT(w, r, "flash.err.bucket_exists")
		return
	}
	if err != nil {
		logFrom(r).Warn("restoreBucketBackup: target bucket refused", "err", err, "bucket", name)
		s.bucketErr(w, r, err)
		return
	}
	t := bucketbackup.Target{Server: di.AppName, Bucket: target.Name, RootUser: di.Superuser, RootPass: di.SuperuserPassword}
	if err := s.bucketBackups.StartRestore(r.Context(), b.ID, snapshot, t, bucketBackupTimeout); err != nil {
		logFrom(r).Warn("restoreBucketBackup: refused", "err", err, "bucket_backup_id", b.ID, "snapshot", snapshot)
		s.dropUnusedRestoreTarget(r, di, target)
		switch {
		case errors.Is(err, bucketbackup.ErrRunning):
			s.flashErrT(w, r, "flash.err.backup_running")
		case errors.Is(err, bucketbackup.ErrNoSnapshot):
			s.flashErrT(w, r, "flash.err.restore_no_snapshot")
		default:
			s.flashErrErr(w, r, "flash.err.restore_failed", err)
		}
		return
	}
	logFrom(r).Info("bucket restore started", "bucket_backup_id", b.ID, "snapshot", snapshot, "target_bucket_id", target.ID)
	s.flashOK(w, r, "flash.ok.bucket_restore_started")
	http.Redirect(w, r, c.url(), http.StatusSeeOther)
}

// dropUnusedRestoreTarget undoes the target bucket a refused restore created.
// The bucket is published as soon as its row exists, so an app may already
// have been linked to it: under the bucket's own lock (the one addBucketLink
// and deleteBucket take), it is removed only while nothing links to it and it
// is still empty. Anything else keeps the bucket, its key and its row.
func (s *Server) dropUnusedRestoreTarget(r *http.Request, di dbservice.Instance, target db.S3Bucket) {
	ctx := context.WithoutCancel(r.Context())
	lock := oplock.Bucket(target.ID)
	if !oplock.TryAcquire(lock) {
		logFrom(r).Warn("restoreBucketBackup: unused target bucket kept (in use)", "bucket_id", target.ID)
		return
	}
	defer oplock.Release(lock)
	links, err := s.q.ListBucketLinksByBucket(ctx, target.ID)
	if err != nil || len(links) > 0 {
		logFrom(r).Warn("restoreBucketBackup: unused target bucket kept (linked or links unknown)", "err", err, "links", len(links), "bucket_id", target.ID)
		return
	}
	if err := s.dbsvc.RemoveEmptyBucket(ctx, di, target.Name); err != nil {
		logFrom(r).Warn("restoreBucketBackup: unused target bucket kept (not empty or not removable)", "err", err, "bucket", target.Name)
		return
	}
	if err := s.removeGeneratedPrincipals(ctx, di, target); err != nil {
		logFrom(r).Error("restoreBucketBackup: remove unused target key", "err", err, "bucket", target.Name)
		return
	}
	if err := s.q.DeleteS3Bucket(ctx, target.ID); err != nil {
		logFrom(r).Error("restoreBucketBackup: delete unused target row", "err", err, "bucket_id", target.ID)
	}
}

// sameStorage reports whether two destinations write into the same S3 bucket:
// same host and effective port (an explicit default port is the same storage
// as none), whatever the scheme's or host's spelling. An endpoint that does not
// parse is compared as written.
func sameStorage(a, b db.Destination) bool {
	return storageAddr(a.Endpoint) == storageAddr(b.Endpoint) && strings.EqualFold(strings.TrimSpace(a.Bucket), strings.TrimSpace(b.Bucket))
}

func storageAddr(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if !strings.Contains(e, "://") {
		e = "https://" + e
	}
	u, err := url.Parse(e)
	if err != nil || u.Hostname() == "" {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port) + strings.TrimRight(u.EscapedPath(), "/")
}
