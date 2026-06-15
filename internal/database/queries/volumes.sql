-- name: CreateVolume :one
INSERT INTO app_volumes (application_id, name, mount_path)
VALUES ($1, $2, $3)
RETURNING id, application_id, name, mount_path, created_at;

-- name: GetVolume :one
SELECT id, application_id, name, mount_path, created_at
FROM app_volumes WHERE id = $1;

-- name: ListVolumesByApplication :many
SELECT id, application_id, name, mount_path, created_at
FROM app_volumes WHERE application_id = $1 ORDER BY created_at;

-- name: DeleteVolume :exec
DELETE FROM app_volumes WHERE id = $1;
