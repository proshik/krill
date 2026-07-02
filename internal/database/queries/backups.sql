-- name: CreateBackup :one
INSERT INTO backups (postgres_db_id, destination_id, schedule, prefix, retention, enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetBackup :one
SELECT * FROM backups WHERE id = $1;

-- name: ListBackupsByDB :many
SELECT * FROM backups WHERE postgres_db_id = $1 ORDER BY created_at;

-- name: ListEnabledBackups :many
SELECT * FROM backups WHERE enabled = true;

-- name: SetBackupEnabled :exec
UPDATE backups SET enabled = $2 WHERE id = $1;

-- name: SetBackupResult :exec
UPDATE backups SET last_run_at = $2, last_status = $3, last_error = $4 WHERE id = $1;

-- name: DeleteBackup :exec
DELETE FROM backups WHERE id = $1;
