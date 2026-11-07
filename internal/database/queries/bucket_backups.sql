-- name: CreateBucketBackup :one
INSERT INTO bucket_backups (bucket_id, destination_id, schedule, prefix, retention, enabled)
VALUES ($1, $2, $3, $4, $5, true) RETURNING *;

-- name: GetBucketBackup :one
SELECT * FROM bucket_backups WHERE id = $1;

-- name: ListBucketBackupsByBucket :many
SELECT * FROM bucket_backups WHERE bucket_id = $1 ORDER BY created_at;

-- name: ListEnabledBucketBackups :many
SELECT * FROM bucket_backups WHERE enabled = true;

-- name: SetBucketBackupEnabled :exec
UPDATE bucket_backups SET enabled = $2 WHERE id = $1;

-- name: SetBucketBackupResult :exec
UPDATE bucket_backups SET last_run_at = $2, last_status = $3, last_error = $4 WHERE id = $1;

-- name: SetBucketRestoreResult :exec
UPDATE bucket_backups SET last_restore_at = $2, last_restore_status = $3, last_restore_error = $4 WHERE id = $1;

-- name: DeleteBucketBackup :exec
DELETE FROM bucket_backups WHERE id = $1;
