-- name: CreateVolumeBackup :one
INSERT INTO volume_backups (app_volume_id, destination_id, schedule, prefix, retention, enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, app_volume_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at;

-- name: GetVolumeBackup :one
SELECT id, app_volume_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at
FROM volume_backups WHERE id = $1;

-- name: ListVolumeBackupsByVolume :many
SELECT id, app_volume_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at
FROM volume_backups WHERE app_volume_id = $1 ORDER BY created_at;

-- name: ListEnabledVolumeBackups :many
SELECT id, app_volume_id, destination_id, schedule, prefix, retention, enabled, last_run_at, last_status, last_error, created_at
FROM volume_backups WHERE enabled = true;

-- name: SetVolumeBackupEnabled :exec
UPDATE volume_backups SET enabled = $2 WHERE id = $1;

-- name: SetVolumeBackupResult :exec
UPDATE volume_backups SET last_run_at = $2, last_status = $3, last_error = $4 WHERE id = $1;

-- name: DeleteVolumeBackup :exec
DELETE FROM volume_backups WHERE id = $1;

-- name: GetVolTarget :one
SELECT av.id AS app_volume_id, av.application_id, av.name AS volume_name, a.replicas
FROM app_volumes av JOIN applications a ON av.application_id = a.id
WHERE av.id = $1;
