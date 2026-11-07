package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// bucketLinkFields are the values a bucket link can inject.
var bucketLinkFields = []string{"endpoint", "bucket", "access_key", "secret_key", "region"}

// newBucketKey generates a principal for bucket: krill-<bucket>-<6 hex>.
func newBucketKey(bucket string) (string, string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	name := bucket
	if len(name) > 40 {
		name = name[:40]
	}
	pw, err := genPassword()
	if err != nil {
		return "", "", err
	}
	return "krill-" + strings.TrimSuffix(name, "-") + "-" + hex.EncodeToString(b[:]), pw, nil
}

// s3Instance loads an S3 instance of the org with its decrypted root password
// for bucket management, or reports why it cannot (an i18n key).
func (s *Server) s3Instance(r *http.Request, orgID, id int64) (dbservice.Instance, string) {
	inst, err := s.q.GetDBInstance(r.Context(), id)
	if err != nil || inst.OrganizationID != orgID {
		return dbservice.Instance{}, "flash.err.db_not_found"
	}
	if inst.Engine != "minio" {
		return dbservice.Instance{}, "flash.err.bucket_s3_only"
	}
	pw, derr := secret.Dec(inst.SuperuserPassword)
	if derr != nil {
		logFrom(r).Error("bucket: instance password undecryptable", "err", derr, "instance_id", inst.ID)
		return dbservice.Instance{}, "flash.err.secret_undecryptable"
	}
	di := dbservice.Instance{ID: inst.ID, Engine: inst.Engine, AppName: inst.AppName, Superuser: inst.Superuser, SuperuserPassword: pw}
	if !s.dbsvc.InstanceRunning(r.Context(), di) {
		return dbservice.Instance{}, "flash.err.ldb_instance_down"
	}
	return di, ""
}

// bucketErr flashes a bucket-management failure: the known ones by key, the
// rest with mc's own message (which never carries a secret).
func (s *Server) bucketErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, dbservice.ErrMCMissing):
		s.flashErrT(w, r, "flash.err.bucket_mc_missing")
	case errors.Is(err, dbservice.ErrBucketMissing):
		s.flashErrT(w, r, "flash.err.bucket_missing")
	case errors.Is(err, dbservice.ErrBucketCredentials):
		s.flashErrT(w, r, "flash.err.bucket_credentials")
	default:
		s.flashErrErr(w, r, "flash.err.bucket_failed", err)
	}
}

// createBucket creates a bucket and its restricted principal on an S3 instance
// of the org, or adopts an existing bucket and principal (mode=import), for
// this environment. Admin-only.
func (s *Server) createBucket(w http.ResponseWriter, r *http.Request) {
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
	instID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("instance_id")), 10, 64)
	if err != nil {
		s.flashErrT(w, r, "flash.err.invalid_database")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !dbservice.ValidBucketName(name) {
		s.flashErrT(w, r, "flash.err.bucket_bad_name")
		return
	}
	importing := r.FormValue("mode") == "import"
	var accessKey, secretKey string
	if importing {
		accessKey = strings.TrimSpace(r.FormValue("access_key"))
		secretKey = r.FormValue("secret_key")
		if !dbservice.ValidAccessKey(accessKey) || len(secretKey) < 8 {
			s.flashErrT(w, r, "flash.err.bucket_bad_key")
			return
		}
	}
	di, key := s.s3Instance(r, o.ID, instID)
	if key != "" {
		s.flashErrT(w, r, key)
		return
	}
	var row db.S3Bucket
	if importing {
		row, err = s.addBucket(r, di, e.ID, name, accessKey, secretKey, false)
	} else {
		row, err = s.newKrillBucket(r, di, e.ID, name)
	}
	if errors.Is(err, errBucketTaken) {
		s.flashErrT(w, r, "flash.err.bucket_exists")
		return
	}
	if err != nil {
		logFrom(r).Warn("createBucket: refused", "err", err, "instance_id", di.ID, "bucket", name, "import", importing)
		s.bucketErr(w, r, err)
		return
	}
	logFrom(r).Info("bucket added", "bucket_id", row.ID, "environment_id", e.ID, "instance_id", di.ID, "bucket", name, "import", importing)
	if importing {
		s.flashOK(w, r, "flash.ok.bucket_imported")
	} else {
		s.flashOK(w, r, "flash.ok.bucket_created")
	}
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID)+"/buckets/"+strconv.FormatInt(row.ID, 10), http.StatusSeeOther)
}

// errBucketTaken: Krill already has a bucket of that name on that server.
var errBucketTaken = errors.New("bucket already in Krill")

// newKrillBucket creates a bucket and a generated principal on the server and
// records it for envID.
func (s *Server) newKrillBucket(r *http.Request, di dbservice.Instance, envID int64, name string) (db.S3Bucket, error) {
	ak, sk, err := newBucketKey(name)
	if err != nil {
		return db.S3Bucket{}, err
	}
	return s.addBucket(r, di, envID, name, ak, sk, true)
}

// addBucket records a bucket, then creates it on the server (create) or checks
// the given principal against the existing one (import), dropping the row
// again when the server refuses. The row goes first: its UNIQUE(instance_id,
// name) refuses a second claim on the bucket before anything touches the
// server.
func (s *Server) addBucket(r *http.Request, di dbservice.Instance, envID int64, name, accessKey, secretKey string, create bool) (db.S3Bucket, error) {
	row, err := s.q.CreateS3Bucket(r.Context(), db.CreateS3BucketParams{
		InstanceID: di.ID, EnvironmentID: envID, Name: name, AccessKey: accessKey,
		SecretKey: secret.Enc(secretKey), CreatedByKrill: create, AccessKeyGenerated: create,
	})
	if err != nil {
		logFrom(r).Info("addBucket: insert refused", "err", err, "instance_id", di.ID, "bucket", name)
		return db.S3Bucket{}, errBucketTaken
	}
	b := dbservice.Bucket{Name: name, AccessKey: accessKey, SecretKey: secretKey}
	if create {
		err = s.dbsvc.CreateBucket(r.Context(), di, b)
	} else {
		err = s.dbsvc.ImportBucket(r.Context(), di, b)
	}
	if err != nil {
		if derr := s.q.DeleteS3Bucket(context.WithoutCancel(r.Context()), row.ID); derr != nil {
			logFrom(r).Error("addBucket: rollback row failed", "err", derr, "bucket_id", row.ID)
		}
		return db.S3Bucket{}, err
	}
	return row, nil
}

// bucketCtx is a bucket loaded through the org→project→environment chain.
type bucketCtx struct {
	Org     db.Organization
	Role    string
	Project db.Project
	Env     db.Environment
	Bucket  db.S3Bucket
}

func (c bucketCtx) url() string {
	return envURL(c.Org.ID, c.Project.ID, c.Env.ID) + "/buckets/" + strconv.FormatInt(c.Bucket.ID, 10)
}

// loadBucket parses {bucketID} and verifies the chain (404 on any mismatch).
func (s *Server) loadBucket(w http.ResponseWriter, r *http.Request) (bucketCtx, bool) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return bucketCtx{}, false
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return bucketCtx{}, false
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return bucketCtx{}, false
	}
	id, ok := pathID(r, "bucketID")
	if !ok {
		http.NotFound(w, r)
		return bucketCtx{}, false
	}
	b, err := s.q.GetS3Bucket(r.Context(), id)
	if err != nil || b.EnvironmentID != e.ID {
		logFrom(r).Info("loadBucket: not found in environment", "bucket_id", id, "environment_id", e.ID)
		http.NotFound(w, r)
		return bucketCtx{}, false
	}
	return bucketCtx{Org: o, Role: role, Project: p, Env: e, Bucket: b}, true
}

// bucketDetail shows a bucket, its endpoint, the principal apps get (the
// secret only to admins), a pending rotation and the apps linked to it.
func (s *Server) bucketDetail(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	inst, err := s.q.GetDBInstance(r.Context(), c.Bucket.InstanceID)
	if err != nil {
		logFrom(r).Error("bucketDetail: instance missing", "err", err, "bucket_id", c.Bucket.ID)
		http.NotFound(w, r)
		return
	}
	links, err := s.q.ListBucketLinksByBucket(r.Context(), c.Bucket.ID)
	if err != nil {
		logFrom(r).Error("bucketDetail: list links", "err", err, "bucket_id", c.Bucket.ID)
	}
	v := templates.BucketView{
		Org: c.Org, Role: c.Role, Project: c.Project, Env: c.Env, Bucket: c.Bucket, Instance: inst,
		Base: c.url(), Endpoint: "http://" + inst.AppName + ":9000", AccessKey: c.Bucket.AccessKey,
		PrevAccessKey: c.Bucket.PrevAccessKey,
	}
	for _, l := range links {
		v.Links = append(v.Links, templates.BucketLinkUse{App: l.AppName, VarName: l.VarName, Field: l.Field})
	}
	if c.Role == "owner" || c.Role == "admin" {
		v.Backups = s.bucketBackupViews(r, c.Bucket.ID)
		if dests, err := s.q.ListDestinationsByOrg(r.Context(), c.Org.ID); err == nil {
			v.Destinations = dests
		}
		if sk, derr := secret.Dec(c.Bucket.SecretKey); derr != nil {
			logFrom(r).Error("bucketDetail: secret undecryptable", "err", derr, "bucket_id", c.Bucket.ID)
		} else {
			v.SecretKey = sk
		}
	}
	render(w, r, http.StatusOK, templates.BucketDetail(v))
}

// rotateBucketKey adds a new principal for the bucket and makes it the one
// links inject, keeping the old one working until it is revoked: redeploy the
// linked apps, check them, then revoke. Admin-only.
func (s *Server) rotateBucketKey(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	if !s.lockBucket(w, r, &c) {
		return
	}
	defer oplock.Release(oplock.Bucket(c.Bucket.ID))
	if c.Bucket.PrevAccessKey != "" {
		s.flashErrT(w, r, "flash.err.bucket_rotation_pending")
		return
	}
	di, key := s.s3Instance(r, c.Org.ID, c.Bucket.InstanceID)
	if key != "" {
		s.flashErrT(w, r, key)
		return
	}
	ak, sk, err := newBucketKey(c.Bucket.Name)
	if err != nil {
		logFrom(r).Error("rotateBucketKey: key generation failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if err := s.dbsvc.AddPrincipal(r.Context(), di, c.Bucket.Name, ak, sk); err != nil {
		logFrom(r).Warn("rotateBucketKey: server refused", "err", err, "bucket_id", c.Bucket.ID)
		s.bucketErr(w, r, err)
		return
	}
	// Conditional on no pending previous key: a rotation that lost a race
	// would otherwise overwrite the record of the key still in use.
	n, err := s.q.RotateS3BucketKey(r.Context(), db.RotateS3BucketKeyParams{ID: c.Bucket.ID, AccessKey: ak, SecretKey: secret.Enc(sk)})
	if err != nil || n == 0 {
		logFrom(r).Error("rotateBucketKey: not recorded; removing the new principal", "err", err, "rows", n, "bucket_id", c.Bucket.ID)
		if rerr := s.dbsvc.RemovePrincipal(context.WithoutCancel(r.Context()), di, ak); rerr != nil {
			logFrom(r).Error("rotateBucketKey: compensating removal failed", "err", rerr, "bucket_id", c.Bucket.ID, "access_key", ak)
		}
		if err == nil {
			s.flashErrT(w, r, "flash.err.bucket_rotation_pending")
		} else {
			s.flashErrT(w, r, "flash.err.internal")
		}
		return
	}
	logFrom(r).Info("bucket key rotated", "bucket_id", c.Bucket.ID, "access_key", ak)
	s.flashOK(w, r, "flash.ok.bucket_rotated")
	http.Redirect(w, r, c.url(), http.StatusSeeOther)
}

// revokeBucketPrevKey removes the principal a rotation replaced. Admin-only.
func (s *Server) revokeBucketPrevKey(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	if !s.lockBucket(w, r, &c) {
		return
	}
	defer oplock.Release(oplock.Bucket(c.Bucket.ID))
	if c.Bucket.PrevAccessKey == "" {
		http.Redirect(w, r, c.url(), http.StatusSeeOther)
		return
	}
	di, key := s.s3Instance(r, c.Org.ID, c.Bucket.InstanceID)
	if key != "" {
		s.flashErrT(w, r, key)
		return
	}
	if err := s.dbsvc.RemovePrincipal(r.Context(), di, c.Bucket.PrevAccessKey); err != nil {
		logFrom(r).Warn("revokeBucketPrevKey: server refused", "err", err, "bucket_id", c.Bucket.ID)
		s.bucketErr(w, r, err)
		return
	}
	if err := s.q.ClearS3BucketPrevKey(r.Context(), c.Bucket.ID); err != nil {
		logFrom(r).Error("revokeBucketPrevKey: save failed", "err", err, "bucket_id", c.Bucket.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("bucket previous key revoked", "bucket_id", c.Bucket.ID, "access_key", c.Bucket.PrevAccessKey)
	s.flashOK(w, r, "flash.ok.bucket_revoked")
	http.Redirect(w, r, c.url(), http.StatusSeeOther)
}

// deleteBucket removes a bucket from Krill. It is refused while apps link to
// it. Without destroy_data the bucket and its objects stay on the server
// (it can be imported again) and only the principals Krill generated are
// removed; with it the bucket is emptied and removed. Admin-only.
func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	// Held through the link check, the server-side cleanup and the row
	// delete, so a link created meanwhile cannot outlive the bucket's data.
	if !s.lockBucket(w, r, &c) {
		return
	}
	defer oplock.Release(oplock.Bucket(c.Bucket.ID))
	links, err := s.q.ListBucketLinksByBucket(r.Context(), c.Bucket.ID)
	if err != nil {
		logFrom(r).Error("deleteBucket: list links", "err", err, "bucket_id", c.Bucket.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if len(links) > 0 {
		s.flashErrT(w, r, "flash.err.bucket_linked")
		return
	}
	di, key := s.s3Instance(r, c.Org.ID, c.Bucket.InstanceID)
	if key != "" {
		s.flashErrT(w, r, key)
		return
	}
	destroy := r.FormValue("destroy_data") == "on"
	if destroy {
		if err := s.dbsvc.RemoveBucket(r.Context(), di, c.Bucket.Name); err != nil {
			logFrom(r).Warn("deleteBucket: remove bucket failed", "err", err, "bucket_id", c.Bucket.ID)
			s.bucketErr(w, r, err)
			return
		}
	}
	if err := s.removeGeneratedPrincipals(r.Context(), di, c.Bucket); err != nil {
		logFrom(r).Warn("deleteBucket: principal removal failed", "err", err, "bucket_id", c.Bucket.ID)
		s.bucketErr(w, r, err)
		return
	}
	if err := s.q.DeleteS3Bucket(r.Context(), c.Bucket.ID); err != nil {
		logFrom(r).Error("deleteBucket: delete row failed", "err", err, "bucket_id", c.Bucket.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	s.reloadBucketBackupSchedules() // its backup configurations cascaded away
	logFrom(r).Info("bucket removed", "bucket_id", c.Bucket.ID, "bucket", c.Bucket.Name, "destroy_data", destroy)
	s.flashOK(w, r, "flash.ok.bucket_deleted")
	http.Redirect(w, r, envURL(c.Org.ID, c.Project.ID, c.Env.ID), http.StatusSeeOther)
}

// removeGeneratedPrincipals removes the bucket's principals Krill generated,
// as recorded on the row — never by their name, since an imported key can
// look like a generated one.
func (s *Server) removeGeneratedPrincipals(ctx context.Context, di dbservice.Instance, b db.S3Bucket) error {
	for _, k := range []struct {
		key       string
		generated bool
	}{{b.AccessKey, b.AccessKeyGenerated}, {b.PrevAccessKey, b.PrevAccessKeyGenerated}} {
		if k.key == "" || !k.generated {
			continue
		}
		if err := s.dbsvc.RemovePrincipal(ctx, di, k.key); err != nil {
			return err
		}
	}
	return nil
}

// lockBucket takes the bucket's lock or flashes "busy"; the caller releases.
//
// It also re-reads the bucket into c: the row loaded before the lock may be
// stale — a rotation or delete that held the lock a moment ago may have
// changed its keys or removed it — and every mutation must act on the keys
// and ownership flags as they are now. A bucket gone meanwhile is a 404, with
// nothing changed on the server.
func (s *Server) lockBucket(w http.ResponseWriter, r *http.Request, c *bucketCtx) bool {
	if !oplock.TryAcquire(oplock.Bucket(c.Bucket.ID)) {
		s.flashErrT(w, r, "flash.err.bucket_busy")
		return false
	}
	b, err := s.q.GetS3Bucket(r.Context(), c.Bucket.ID)
	if err != nil {
		oplock.Release(oplock.Bucket(c.Bucket.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			logFrom(r).Error("lockBucket: re-read failed", "err", err, "bucket_id", c.Bucket.ID)
			s.flashErrT(w, r, "flash.err.internal")
		}
		return false
	}
	c.Bucket = b
	return true
}

// removeEnvBuckets runs when an environment is deleted: its buckets' rows go
// with it, so the principals Krill generated for them are removed too. The
// buckets and their objects stay on the server — deleting an environment must
// not destroy object data by implication; an operator can import them again.
func (s *Server) removeEnvBuckets(ctx context.Context, envID int64) {
	if s.dbsvc == nil {
		return
	}
	buckets, err := s.q.ListS3BucketsByEnvironment(ctx, envID)
	if err != nil {
		slog.Error("removeEnvBuckets: list", "environment_id", envID, "err", err)
		return
	}
	for _, b := range buckets {
		inst, err := s.q.GetDBInstance(ctx, b.InstanceID)
		if err != nil {
			slog.Error("removeEnvBuckets: instance lookup", "bucket_id", b.ID, "err", err)
			continue
		}
		pw, derr := secret.Dec(inst.SuperuserPassword)
		if derr != nil {
			slog.Error("removeEnvBuckets: instance password undecryptable (principals left in place)", "bucket_id", b.ID, "err", derr)
			continue
		}
		di := dbservice.Instance{ID: inst.ID, AppName: inst.AppName, Superuser: inst.Superuser, SuperuserPassword: pw}
		row := db.S3Bucket{AccessKey: b.AccessKey, PrevAccessKey: b.PrevAccessKey,
			AccessKeyGenerated: b.AccessKeyGenerated, PrevAccessKeyGenerated: b.PrevAccessKeyGenerated}
		if err := s.removeGeneratedPrincipals(ctx, di, row); err != nil {
			slog.Error("removeEnvBuckets: principal removal failed (left in place)", "bucket_id", b.ID, "err", err)
		}
	}
}

// addBucketLink links a bucket of the app's environment to an env var,
// injected at deploy with the bucket's restricted principal. Admin-only.
func (s *Server) addBucketLink(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	bucketID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("bucket_id")), 10, 64)
	if err != nil {
		s.flashErrT(w, r, "flash.err.invalid_database")
		return
	}
	b, err := s.q.GetS3Bucket(r.Context(), bucketID)
	if err != nil || b.EnvironmentID != c.Env.ID {
		logFrom(r).Info("addBucketLink: bucket not in app environment", "bucket_id", bucketID, "app_id", c.App.ID)
		http.NotFound(w, r)
		return
	}
	// The bucket's lock: a delete checks for links and then removes the data,
	// and a link created in between would survive its bucket.
	if !oplock.TryAcquire(oplock.Bucket(b.ID)) {
		s.flashErrT(w, r, "flash.err.bucket_busy")
		return
	}
	defer oplock.Release(oplock.Bucket(b.ID))
	// Re-read under the lock: a delete that held it a moment ago may have
	// removed the bucket after the first read.
	if _, err := s.q.GetS3Bucket(r.Context(), b.ID); err != nil {
		http.NotFound(w, r)
		return
	}
	varName := strings.TrimSpace(r.FormValue("var_name"))
	if !envVarNameRe.MatchString(varName) {
		s.flashErrT(w, r, "flash.err.invalid_var_name")
		return
	}
	field := r.FormValue("field")
	valid := false
	for _, f := range bucketLinkFields {
		valid = valid || f == field
	}
	if !valid {
		s.flashErrT(w, r, "flash.err.invalid_field")
		return
	}
	if key := s.linkVarTaken(r, c.App, varName); key != "" {
		s.flashErr(w, r, i18n.Tf(r.Context(), key, varName))
		return
	}
	if _, err := s.q.CreateBucketLink(r.Context(), db.CreateBucketLinkParams{
		ApplicationID: c.App.ID, BucketID: b.ID, VarName: varName, Field: field,
	}); err != nil {
		logFrom(r).Error("addBucketLink: create failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.link_var_exists")
		return
	}
	logFrom(r).Info("bucket link created", "app_id", c.App.ID, "bucket_id", b.ID, "var", varName, "field", field)
	s.flashOK(w, r, "flash.ok.db_linked")
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}

// linkVarTaken returns the i18n key (taking the var name) of why varName
// cannot be a new link, or "": it must not shadow env_text, the metrics token
// variable or another link of either kind.
func (s *Server) linkVarTaken(r *http.Request, app db.Application, varName string) string {
	existing, _ := parseEnv(app.EnvText)
	if _, dup := existing[varName]; dup {
		return "flash.err.var_in_env"
	}
	if app.MetricsEnabled && varName == app.MetricsTokenEnv {
		return "flash.err.metrics_env_taken"
	}
	dbLinks, err := s.q.ListDBLinksByApplication(r.Context(), app.ID)
	if err != nil {
		logFrom(r).Error("link var check: list db links", "err", err, "app_id", app.ID)
		return "flash.err.link_var_taken"
	}
	for _, l := range dbLinks {
		if l.VarName == varName {
			return "flash.err.link_var_taken"
		}
	}
	bLinks, err := s.q.ListBucketLinksByApplication(r.Context(), app.ID)
	if err != nil {
		logFrom(r).Error("link var check: list bucket links", "err", err, "app_id", app.ID)
		return "flash.err.link_var_taken"
	}
	for _, l := range bLinks {
		if l.VarName == varName {
			return "flash.err.link_var_taken"
		}
	}
	return ""
}

func (s *Server) deleteBucketLink(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r, "linkID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	l, err := s.q.GetBucketLink(r.Context(), id)
	if err != nil || l.ApplicationID != c.App.ID {
		logFrom(r).Info("deleteBucketLink: not found in app", "link_id", id, "app_id", c.App.ID)
		http.NotFound(w, r)
		return
	}
	if err := s.q.DeleteBucketLink(r.Context(), l.ID); err != nil {
		logFrom(r).Error("deleteBucketLink: delete failed", "err", err, "link_id", l.ID)
		s.flashErrT(w, r, "flash.err.remove_link")
		return
	}
	logFrom(r).Info("bucket link removed", "app_id", c.App.ID, "link_id", l.ID, "var", l.VarName)
	s.flashOK(w, r, "flash.ok.link_removed")
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}
