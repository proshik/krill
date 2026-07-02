-- name: GetNotificationChannel :one
SELECT id, org_id, type, enabled, bot_token, chat_id, notify_deploy, notify_backup, notify_health, created_at, updated_at
FROM notification_channels WHERE org_id = $1 AND type = $2;

-- name: UpsertNotificationChannel :one
INSERT INTO notification_channels (org_id, type, enabled, bot_token, chat_id, notify_deploy, notify_backup, notify_health)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (org_id, type) DO UPDATE SET
    enabled = excluded.enabled,
    bot_token = excluded.bot_token,
    chat_id = excluded.chat_id,
    notify_deploy = excluded.notify_deploy,
    notify_backup = excluded.notify_backup,
    notify_health = excluded.notify_health,
    updated_at = now()
RETURNING id, org_id, type, enabled, bot_token, chat_id, notify_deploy, notify_backup, notify_health, created_at, updated_at;

-- name: ChannelsForOrg :many
SELECT id, org_id, type, enabled, bot_token, chat_id, notify_deploy, notify_backup, notify_health, created_at, updated_at
FROM notification_channels WHERE org_id = $1 AND enabled;

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
