package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	// LastRunAt is when the run that recorded the last result started.
	LastRunAt  string `json:"last_run_at,omitempty"`
	LastStatus string `json:"last_status,omitempty"` // ok | error
	LastError  string `json:"last_error,omitempty"`  // redacted, bounded
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

// BackupStarted answers BackupNow. The run's outcome is the list entry's
// result once running is false and its last_run_at is not before StartedAt.
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
	for _, r := range okRuns {
		lastOK[r.Kind+":"+strconv.FormatInt(r.SourceID, 10)] = r.FinishedAt
	}
	common := func(kind string, id int64, schedule string, retention int32, enabled bool, lastRun pgtype.Timestamptz, status, lastErr, dest string) BackupInfo {
		b := BackupInfo{
			Kind: kind, ID: id, Destination: dest, Schedule: schedule, Enabled: enabled, Retention: retention,
			LastStatus: status, LastError: redactBackupError(lastErr),
		}
		if s.backups != nil {
			b.Running = s.backups.Running(kind, id)
		}
		if lastRun.Valid {
			b.LastRunAt = formatTime(lastRun.Time)
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
	now := time.Now()
	if err := s.backups.Start(kind, backupID, now); err != nil {
		switch {
		case errors.Is(err, ErrBackupBusy):
			return BackupStarted{}, Conflict("a backup or restore of it is already running; follow it with krill_list_backups")
		case errors.Is(err, ErrBackupUnavailable):
			return BackupStarted{}, Unavailable("backups of this kind are unavailable on this server")
		}
		return BackupStarted{}, fmt.Errorf("backup now: %w", err)
	}
	return BackupStarted{Kind: kind, ID: backupID, Status: "started", StartedAt: formatTime(now)}, nil
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

// backupErrorMax bounds a returned backup error.
const backupErrorMax = 500

var (
	// urlUserinfo matches the credentials of a URL (scheme://user:pass@).
	urlUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s@]+@`)
	// credentialPair matches key=value or key: value with a credential-like
	// key, also as a query parameter (X-Amz-Signature=…, password=…).
	credentialPair = regexp.MustCompile(`(?i)((?:x-amz-)?(?:password|passwd|pwd|secret[a-z_-]*|token|signature|credential|access[_-]?key[a-z_-]*|api[_-]?key|authorization)\s*[=:]\s*)("[^"]*"|[^\s&"',;]+)`)
)

// redactBackupError makes a stored backup error safe to hand an agent: the
// text comes from SDKs and tools and is not guaranteed free of credentials.
// URL userinfo and credential-named values are masked; the result is cut to
// backupErrorMax bytes on a character boundary.
func redactBackupError(msg string) string {
	if msg == "" {
		return ""
	}
	msg = urlUserinfo.ReplaceAllString(msg, "${1}***@")
	msg = credentialPair.ReplaceAllString(msg, "${1}***")
	if len(msg) > backupErrorMax {
		cut := backupErrorMax
		for cut > 0 && msg[cut]&0xC0 == 0x80 {
			cut--
		}
		msg = strings.TrimSpace(msg[:cut]) + "…"
	}
	return msg
}
