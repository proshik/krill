-- name: CreateVolume :one
INSERT INTO app_volumes (application_id, name, mount_path, owner)
VALUES ($1, $2, $3, $4)
RETURNING id, application_id, name, mount_path, created_at, owner;

-- name: GetVolume :one
SELECT id, application_id, name, mount_path, created_at, owner
FROM app_volumes WHERE id = $1;

-- name: ListVolumesByApplication :many
SELECT id, application_id, name, mount_path, created_at, owner
FROM app_volumes WHERE application_id = $1 ORDER BY created_at;

-- name: DeleteVolume :exec
DELETE FROM app_volumes WHERE id = $1;

-- name: SetVolumeOwner :exec
UPDATE app_volumes SET owner = $2 WHERE id = $1;
