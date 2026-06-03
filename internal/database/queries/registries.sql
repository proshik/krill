-- name: CreateRegistry :one
INSERT INTO registries (organization_id, name, registry_url, username, password)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, organization_id, name, registry_url, username, password, created_at;

-- name: GetRegistry :one
SELECT id, organization_id, name, registry_url, username, password, created_at
FROM registries WHERE id = $1;

-- name: ListRegistriesByOrg :many
SELECT id, organization_id, name, registry_url, username, password, created_at
FROM registries WHERE organization_id = $1 ORDER BY name;

-- name: DeleteRegistry :exec
DELETE FROM registries WHERE id = $1;

-- name: CountRegistriesByName :one
SELECT count(*) FROM registries WHERE organization_id = $1 AND name = $2;

-- name: CountApplicationsByRegistry :one
SELECT count(*) FROM applications WHERE registry_id = $1;
