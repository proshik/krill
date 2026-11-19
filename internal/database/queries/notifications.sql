-- name: GetNotificationChannel :one
SELECT * FROM notification_channels WHERE org_id = $1 AND type = $2;

-- name: UpsertNotificationChannel :one
INSERT INTO notification_channels (
    org_id, type, enabled, bot_token, chat_id, notify_deploy, notify_backup, notify_health,
    notify_deploy_success, notify_infra, disk_threshold_pct, mem_threshold_pct,
    digest_enabled, digest_minute, digest_tz, lang)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (org_id, type) DO UPDATE SET
    enabled = excluded.enabled,
    bot_token = excluded.bot_token,
    chat_id = excluded.chat_id,
    notify_deploy = excluded.notify_deploy,
    notify_backup = excluded.notify_backup,
    notify_health = excluded.notify_health,
    notify_deploy_success = excluded.notify_deploy_success,
    notify_infra = excluded.notify_infra,
    disk_threshold_pct = excluded.disk_threshold_pct,
    mem_threshold_pct = excluded.mem_threshold_pct,
    digest_enabled = excluded.digest_enabled,
    digest_minute = excluded.digest_minute,
    digest_tz = excluded.digest_tz,
    lang = excluded.lang,
    updated_at = now()
RETURNING *;

-- name: ChannelsForOrg :many
SELECT * FROM notification_channels WHERE org_id = $1 AND enabled;

-- name: CountEnabledHealthChannels :one
SELECT count(*) FROM notification_channels WHERE enabled AND notify_health;

-- name: AppNotifyTarget :one
SELECT p.organization_id AS org_id, p.name AS project_name, e.name AS env_name, a.name AS app_name
FROM applications a
JOIN environments e ON a.environment_id = e.id
JOIN projects p ON e.project_id = p.id
WHERE a.id = $1;

-- name: BackupNotifyTarget :one
SELECT p.organization_id AS org_id, p.name AS project_name, e.name AS env_name, di.app_name AS db_name
FROM backups b
JOIN logical_databases ld ON b.logical_database_id = ld.id
JOIN db_instances di ON ld.instance_id = di.id
JOIN environments e ON ld.environment_id = e.id
JOIN projects p ON e.project_id = p.id
WHERE b.id = $1;

-- name: ListWatchedApps :many
-- Returns ALL apps across ALL orgs; used only by the internal health watcher.
-- Never expose these rows in a user/org-scoped handler without re-filtering (IDOR).
SELECT a.id AS app_id, p.organization_id AS org_id, a.name AS app_name, e.name AS env_name, p.name AS project_name
FROM applications a
JOIN environments e ON a.environment_id = e.id
JOIN projects p ON e.project_id = p.id;

-- name: VolumeBackupNotifyTarget :one
SELECT p.organization_id AS org_id, p.name AS project_name, e.name AS env_name, (a.name || ' / ' || av.name)::text AS target_name
FROM volume_backups vb
JOIN app_volumes av ON vb.app_volume_id = av.id
JOIN applications a ON av.application_id = a.id
JOIN environments e ON a.environment_id = e.id
JOIN projects p ON e.project_id = p.id
WHERE vb.id = $1;

-- name: BucketBackupNotifyTarget :one
SELECT p.organization_id AS org_id, p.name AS project_name, e.name AS env_name, b.name AS target_name
FROM bucket_backups bb
JOIN s3_buckets b ON bb.bucket_id = b.id
JOIN environments e ON b.environment_id = e.id
JOIN projects p ON e.project_id = p.id
WHERE bb.id = $1;

-- name: InfraChannels :many
-- Enabled channels of EVERY organization that an instance operator opted into
-- node-level alerts. Used only by the internal resource monitor.
SELECT * FROM notification_channels WHERE enabled AND notify_infra;

-- name: DigestChannels :many
-- Enabled channels with the daily digest on, across all organizations; used
-- only by the internal digest scheduler, which scopes each digest to its org.
SELECT * FROM notification_channels WHERE enabled AND digest_enabled;

-- name: ClaimDigest :execrows
-- Marks a channel's digest as sent at $2 unless another pass already claimed
-- this occurrence (digest_last_at at or after the scheduled time).
UPDATE notification_channels SET digest_last_at = sqlc.arg(claimed_at)::timestamptz
WHERE id = sqlc.arg(id) AND (digest_last_at IS NULL OR digest_last_at < sqlc.arg(scheduled_at)::timestamptz);

-- name: ReleaseDigest :exec
-- Undoes a claim whose send failed, so the next pass retries it. Only while
-- the claim is still ours.
UPDATE notification_channels SET digest_last_at = sqlc.narg(previous)
WHERE id = sqlc.arg(id) AND digest_last_at = sqlc.arg(claimed_at)::timestamptz;

-- name: GetAlertState :one
SELECT * FROM notify_alert_state WHERE channel_id = $1 AND alert_key = $2;

-- name: SetAlertState :exec
INSERT INTO notify_alert_state (channel_id, alert_key, active, last_sent_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (channel_id, alert_key) DO UPDATE
  SET active = EXCLUDED.active, last_sent_at = EXCLUDED.last_sent_at;

-- name: InsertBackupRun :exec
INSERT INTO backup_runs (org_id, kind, source_id, project, env, target, status, size_bytes, error, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListBackupRunsSince :many
SELECT * FROM backup_runs WHERE org_id = $1 AND finished_at >= $2 ORDER BY finished_at;

-- name: PruneBackupRuns :exec
DELETE FROM backup_runs WHERE finished_at < $1;

-- name: DeploymentNotifyInfo :one
SELECT d.id, d.application_id, d.status, d.trigger, d.image_tag, d.started_at, d.finished_at,
       p.organization_id AS org_id, p.name AS project_name, e.name AS env_name, a.name AS app_name
FROM deployments d
JOIN applications a ON d.application_id = a.id
JOIN environments e ON a.environment_id = e.id
JOIN projects p ON e.project_id = p.id
WHERE d.id = $1;

-- name: ListOrgDeploymentsSince :many
-- An organization's deployments that finished since $2 (the digest window).
SELECT d.id, d.application_id, d.status, d.trigger, d.image_tag, d.error_message, d.started_at, d.finished_at,
       p.name AS project_name, e.name AS env_name, a.name AS app_name
FROM deployments d
JOIN applications a ON d.application_id = a.id
JOIN environments e ON a.environment_id = e.id
JOIN projects p ON e.project_id = p.id
WHERE p.organization_id = $1 AND d.finished_at >= $2
ORDER BY d.finished_at;

-- name: ListOrgApps :many
SELECT a.id AS app_id, a.name AS app_name, e.name AS env_name, p.name AS project_name
FROM applications a
JOIN environments e ON a.environment_id = e.id
JOIN projects p ON e.project_id = p.id
WHERE p.organization_id = $1
ORDER BY p.name, e.name, a.name;
