package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Backup kinds, the names backup_runs.kind uses.
const (
	BackupDatabase = "database" // pg_dump of a logical database
	BackupVolume   = "volume"   // archive of an app volume
	BackupBucket   = "bucket"   // snapshot of an S3 bucket
)

var (
	// ErrBackupBusy: a backup or restore of that configuration is running.
	ErrBackupBusy = errors.New("a backup or restore of it is already running")
	// ErrBackupUnavailable: that kind of backup is not wired on this server.
	ErrBackupUnavailable = errors.New("backups of this kind are unavailable")
)

// Backups starts configured backups, as the web UI's "Back up now" does, and
// reports which of them run. The server implements it over the three backup
// services, so the same double-run guard applies to both.
type Backups interface {
	// Start begins a backup of kind/id in the background (ErrBackupBusy,
	// ErrBackupUnavailable).
	Start(kind string, id int64, now time.Time) error
	// Running reports a backup or restore of kind/id under way.
	Running(kind string, id int64) bool
}

// BackupInfo is one configured backup and its last result.
type BackupInfo struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
	// Target is what is backed up: project/env/database, project/env/app:volume
	// or project/env/bucket.
	Target      string `json:"target"`
	Server      string `json:"server,omitempty"` // the database server, for database and bucket backups
	Destination string `json:"destination"`      // the S3 destination's name
	Schedule    string `json:"schedule"`
	Enabled     bool   `json:"enabled"`
	Retention   int32  `json:"retention"`
	Running     bool   `json:"running"`
	// LastRunAt is when the run that recorded the last result started, to
	// the microsecond: the run BackupNow started is the one whose
	// LastRunAt equals its StartedAt.
	LastRunAt  string `json:"last_run_at,omitempty"`
	LastStatus string `json:"last_status,omitempty"` // ok | error
	// LastError is a summary Krill writes of the stored error, never the
	// stored text itself: that comes from SDKs and tools and can carry a
	// credential. The full text is on the backup's page in the web UI.
	LastError string `json:"last_error,omitempty"`
	// LastSuccessAt is when the latest successful run on record finished
	// (runs are kept 30 days), or, when the last run succeeded, when it ran.
	LastSuccessAt string `json:"last_success_at,omitempty"`
}

// BackupFilter narrows ListBackups: App to the backups of one app's volumes
// and of the databases and buckets it is linked to, Environment
// (project/environment) to one environment. Both empty: the organization.
type BackupFilter struct {
	App         string
	Environment string
}

// BackupStarted answers BackupNow. Once the list entry's running is false,
// its result is this run's when last_run_at equals StartedAt, a later run's
// when it is later, and none (this run recorded nothing) when it is earlier.
type BackupStarted struct {
	Kind      string `json:"kind"`
	ID        int64  `json:"id"`
	Status    string `json:"status"` // started
	StartedAt string `json:"started_at"`
}

// backupRow is a list row of any kind before filtering.
type backupRow struct {
	info  BackupInfo
	envID int64
	appID int64 // volume backups only
	ldbID int64 // database backups only
	bkID  int64 // bucket backups only
}

// ListBackups lists the organization's configured backups of every kind.
func (s *Service) ListBackups(ctx context.Context, id Identity, f BackupFilter) ([]BackupInfo, error) {
	if f.App != "" && f.Environment != "" {
		return nil, Invalid("filter by app or by environment, not both")
	}
	rows, err := s.backupRows(ctx, id.OrgID)
	if err != nil {
		return nil, err
	}
	keep := func(backupRow) bool { return true }
	switch {
	case f.Environment != "":
		env, err := s.resolveEnv(ctx, id, f.Environment)
		if err != nil {
			return nil, err
		}
		keep = func(r backupRow) bool { return r.envID == env.EnvID }
	case f.App != "":
		app, err := s.resolveApp(ctx, id, f.App)
		if err != nil {
			return nil, err
		}
		ldbs, bks, err := s.appLinkedStores(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		keep = func(r backupRow) bool {
			switch r.info.Kind {
			case BackupVolume:
				return r.appID == app.ID
			case BackupDatabase:
				return ldbs[r.ldbID]
			default:
				return bks[r.bkID]
			}
		}
	}
	out := []BackupInfo{}
	for _, r := range rows {
		if keep(r) {
			out = append(out, r.info)
		}
	}
	return out, nil
}

// appLinkedStores is the logical databases and buckets an app is linked to.
func (s *Service) appLinkedStores(ctx context.Context, appID int64) (ldbs, buckets map[int64]bool, err error) {
	ldbs, buckets = map[int64]bool{}, map[int64]bool{}
	links, err := s.q.ListDBLinksByApplication(ctx, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("list backups: db links: %w", err)
	}
	for _, l := range links {
		if l.LogicalDatabaseID != nil {
			ldbs[*l.LogicalDatabaseID] = true
		}
	}
	blinks, err := s.q.ListBucketLinksByApplication(ctx, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("list backups: bucket links: %w", err)
	}
	for _, l := range blinks {
		buckets[l.BucketID] = true
	}
	return ldbs, buckets, nil
}

// backupRows reads every backup of the organization, of all three kinds.
func (s *Service) backupRows(ctx context.Context, orgID int64) ([]backupRow, error) {
	okRuns, err := s.q.ListLatestOkBackupRuns(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list backups: latest runs: %w", err)
	}
	lastOK := make(map[string]time.Time, len(okRuns))
	runningBefore, err := s.runningBackups(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, r := range okRuns {
		lastOK[r.Kind+":"+strconv.FormatInt(r.SourceID, 10)] = r.FinishedAt
	}
	common := func(kind string, id int64, schedule string, retention int32, enabled bool, lastRun pgtype.Timestamptz, status, lastErr, dest string) BackupInfo {
		b := BackupInfo{
			Kind: kind, ID: id, Destination: dest, Schedule: schedule, Enabled: enabled, Retention: retention,
			LastStatus: status, LastError: backupErrorSummary(lastErr),
		}
		// Running is sampled before the rows were read and again now: a run
		// that ended in between may have written its result after the read,
		// so it still counts as running and the caller looks again, rather
		// than pairing running=false with the previous result.
		b.Running = runningBefore[kind+":"+strconv.FormatInt(id, 10)] || (s.backups != nil && s.backups.Running(kind, id))
		if lastRun.Valid {
			b.LastRunAt = formatMicro(lastRun.Time)
		}
		success, ok := lastOK[kind+":"+strconv.FormatInt(id, 10)]
		if status == "ok" && lastRun.Valid && (!ok || lastRun.Time.After(success)) {
			success, ok = lastRun.Time, true
		}
		if ok {
			b.LastSuccessAt = formatTime(success)
		}
		return b
	}

	var out []backupRow
	dbs, err := s.q.ListOrgDatabaseBackups(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list backups: databases: %w", err)
	}
	for _, r := range dbs {
		b := common(BackupDatabase, r.ID, r.Schedule, r.Retention, r.Enabled, r.LastRunAt, r.LastStatus, r.LastError, r.DestinationName)
		b.Target, b.Server = r.ProjectName+"/"+r.EnvironmentName+"/"+r.DatabaseName, r.ServerName
		out = append(out, backupRow{info: b, envID: r.EnvironmentID, ldbID: r.LogicalDatabaseID})
	}
	vols, err := s.q.ListOrgVolumeBackups(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list backups: volumes: %w", err)
	}
	for _, r := range vols {
		b := common(BackupVolume, r.ID, r.Schedule, r.Retention, r.Enabled, r.LastRunAt, r.LastStatus, r.LastError, r.DestinationName)
		b.Target = r.ProjectName + "/" + r.EnvironmentName + "/" + r.ApplicationName + ":" + r.VolumeName
		out = append(out, backupRow{info: b, envID: r.EnvironmentID, appID: r.ApplicationID})
	}
	bks, err := s.q.ListOrgBucketBackups(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list backups: buckets: %w", err)
	}
	for _, r := range bks {
		b := common(BackupBucket, r.ID, r.Schedule, r.Retention, r.Enabled, r.LastRunAt, r.LastStatus, r.LastError, r.DestinationName)
		b.Target, b.Server = r.ProjectName+"/"+r.EnvironmentName+"/"+r.BucketName, r.ServerName
		out = append(out, backupRow{info: b, envID: r.EnvironmentID, bkID: r.BucketID})
	}
	return out, nil
}

// BackupNow starts an existing backup of the organization in the background,
// exactly as the web UI's "Back up now" button does.
func (s *Service) BackupNow(ctx context.Context, id Identity, kind string, backupID int64) (BackupStarted, error) {
	// The gate comes before any lookup, so a read token cannot tell a
	// foreign backup from one of its own.
	if err := requireWrite(id); err != nil {
		return BackupStarted{}, err
	}
	switch kind {
	case BackupDatabase, BackupVolume, BackupBucket:
	default:
		return BackupStarted{}, Invalid(fmt.Sprintf("unknown backup kind %q: use database, volume or bucket", kind))
	}
	if err := s.backupInOrg(ctx, id.OrgID, kind, backupID); err != nil {
		return BackupStarted{}, err
	}
	if s.backups == nil {
		return BackupStarted{}, Unavailable("backups are unavailable on this server")
	}
	// Microsecond precision: the stored last_run_at of this run is this
	// value exactly, which is how a caller recognizes its result.
	now := time.Now().Truncate(time.Microsecond)
	if err := s.backups.Start(kind, backupID, now); err != nil {
		switch {
		case errors.Is(err, ErrBackupBusy):
			return BackupStarted{}, Conflict("a backup or restore of it is already running; follow it with krill_list_backups")
		case errors.Is(err, ErrBackupUnavailable):
			return BackupStarted{}, Unavailable("backups of this kind are unavailable on this server")
		}
		return BackupStarted{}, fmt.Errorf("backup now: %w", err)
	}
	return BackupStarted{Kind: kind, ID: backupID, Status: "started", StartedAt: formatMicro(now)}, nil
}

// formatMicro renders t to the microsecond, the precision PostgreSQL keeps,
// so a stored run time compares equal to the time the run was started with.
func formatMicro(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z07:00")
}

// runningBackups samples which of the organization's backups run, before
// their rows are read (see backupRows).
func (s *Service) runningBackups(ctx context.Context, orgID int64) (map[string]bool, error) {
	out := map[string]bool{}
	if s.backups == nil {
		return out, nil
	}
	ids, err := s.q.ListOrgBackupIDs(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list backups: ids: %w", err)
	}
	for _, r := range ids {
		if s.backups.Running(r.Kind, r.ID) {
			out[r.Kind+":"+strconv.FormatInt(r.ID, 10)] = true
		}
	}
	return out, nil
}

// backupInOrg checks that a backup configuration exists in the organization;
// anything else is not found.
func (s *Service) backupInOrg(ctx context.Context, orgID int64, kind string, backupID int64) error {
	notFound := NotFound(fmt.Sprintf("no %s backup %d in this organization", kind, backupID))
	var envID int64
	switch kind {
	case BackupDatabase:
		b, err := s.q.GetBackup(ctx, backupID)
		if err != nil {
			return notFound
		}
		ld, err := s.q.GetLogicalDatabase(ctx, b.LogicalDatabaseID)
		if err != nil {
			return notFound
		}
		envID = ld.EnvironmentID
	case BackupVolume:
		vb, err := s.q.GetVolumeBackup(ctx, backupID)
		if err != nil {
			return notFound
		}
		v, err := s.q.GetVolume(ctx, vb.AppVolumeID)
		if err != nil {
			return notFound
		}
		chain, err := s.q.GetApplicationChain(ctx, v.ApplicationID)
		if err != nil || chain.OrgID != orgID {
			return notFound
		}
		return nil
	case BackupBucket:
		bb, err := s.q.GetBucketBackup(ctx, backupID)
		if err != nil {
			return notFound
		}
		bk, err := s.q.GetS3Bucket(ctx, bb.BucketID)
		if err != nil {
			return notFound
		}
		envID = bk.EnvironmentID
	}
	env, err := s.q.GetEnvironment(ctx, envID)
	if err != nil {
		return notFound
	}
	proj, err := s.q.GetProject(ctx, env.ProjectID)
	if err != nil || proj.OrganizationID != orgID {
		return notFound
	}
	return nil
}

// backupErrorSummary turns a stored backup error into a summary Krill
// writes itself. The stored text comes from S3 SDKs, pg_dump and archive
// tools and can echo a credential (an access key, a signed URL); none of it
// is passed on, only the kind of failure it shows.
func backupErrorSummary(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	m := strings.ToLower(msg)
	has := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(m, sub) {
				return true
			}
		}
		return false
	}
	switch {
	case strings.HasPrefix(m, "backup stored, but"):
		return "the backup was stored, but old backups could not be pruned: the destination keeps growing"
	case has("migrating"):
		return "the database server was migrating; the backup was not taken"
	case has("context deadline exceeded", "timeout", "timed out"):
		return "the backup ran out of time"
	case has("accessdenied", "access denied", "signaturedoesnotmatch", "invalidaccesskeyid", "statuscode: 403", "status code: 403", "forbidden"):
		return "the destination refused the credentials"
	case has("nosuchbucket", "no such bucket"):
		return "the destination bucket does not exist"
	case has("no such host", "connection refused", "network is unreachable", "dial tcp", "egress"):
		return "the destination could not be reached"
	case has("undecryptable"):
		return "a stored credential could not be decrypted (missing or rotated KRILL_SECRET_KEY)"
	case has("pg_dump"):
		return "pg_dump failed"
	case has("no running container", "not running"):
		return "the source was not running"
	}
	return "the backup failed; the full error is on the backup's page in the web UI"
}
