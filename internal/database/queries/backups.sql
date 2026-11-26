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

-- name: ListOrgDatabaseBackups :many
-- Every pg_dump backup of an organization's logical databases, with where it
-- lives.
SELECT b.id, b.schedule, b.retention, b.enabled, b.last_run_at, b.last_status, b.last_error,
       ld.id AS logical_database_id, ld.name AS database_name, i.name AS server_name,
       e.id AS environment_id, e.name AS environment_name, p.name AS project_name,
       d.name AS destination_name
FROM backups b
JOIN logical_databases ld ON ld.id = b.logical_database_id
JOIN db_instances i ON i.id = ld.instance_id
JOIN environments e ON e.id = ld.environment_id
JOIN projects p ON p.id = e.project_id
JOIN destinations d ON d.id = b.destination_id
WHERE p.organization_id = $1
ORDER BY p.name, e.name, ld.name, b.id;

-- name: ListOrgBackupIDs :many
-- The kind and id of every backup configuration of an organization.
SELECT 'database'::text AS kind, b.id FROM backups b
JOIN logical_databases ld ON ld.id = b.logical_database_id
JOIN environments e ON e.id = ld.environment_id
JOIN projects p ON p.id = e.project_id
WHERE p.organization_id = $1
UNION ALL
SELECT 'volume'::text, vb.id FROM volume_backups vb
JOIN app_volumes v ON v.id = vb.app_volume_id
JOIN applications a ON a.id = v.application_id
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
WHERE p.organization_id = $1
UNION ALL
SELECT 'bucket'::text, bb.id FROM bucket_backups bb
JOIN s3_buckets bk ON bk.id = bb.bucket_id
JOIN environments e ON e.id = bk.environment_id
JOIN projects p ON p.id = e.project_id
WHERE p.organization_id = $1;
