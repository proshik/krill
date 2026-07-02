-- name: CreateBackup :one
INSERT INTO backups (logical_database_id, destination_id, schedule, prefix, retention, enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, logical_database_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at;

-- name: GetBackup :one
SELECT id, logical_database_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at
FROM backups WHERE id = $1;

-- name: ListBackupsByLogicalDB :many
SELECT id, logical_database_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at
FROM backups WHERE logical_database_id = $1 ORDER BY created_at;

-- name: ListEnabledBackups :many
SELECT id, logical_database_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at
FROM backups WHERE enabled = true;

-- name: SetBackupEnabled :exec
UPDATE backups SET enabled = $2 WHERE id = $1;

-- name: SetBackupResult :exec
UPDATE backups SET last_run_at = $2, last_status = $3, last_error = $4 WHERE id = $1;

-- name: DeleteBackup :exec
DELETE FROM backups WHERE id = $1;
