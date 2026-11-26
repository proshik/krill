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

-- name: ListOrgBucketBackups :many
-- Every backup of an organization's buckets (see ListOrgDatabaseBackups).
SELECT bb.id, bb.schedule, bb.retention, bb.enabled, bb.last_run_at, bb.last_status, bb.last_error,
       bk.id AS bucket_id, bk.name AS bucket_name, i.name AS server_name,
       e.id AS environment_id, e.name AS environment_name, p.name AS project_name,
       d.name AS destination_name
FROM bucket_backups bb
JOIN s3_buckets bk ON bk.id = bb.bucket_id
JOIN db_instances i ON i.id = bk.instance_id
JOIN environments e ON e.id = bk.environment_id
JOIN projects p ON p.id = e.project_id
JOIN destinations d ON d.id = bb.destination_id
WHERE p.organization_id = $1
ORDER BY p.name, e.name, bk.name, bb.id;
