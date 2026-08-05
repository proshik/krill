-- name: CreateEnvironment :one
INSERT INTO environments (project_id, name, slug) VALUES ($1, $2, $3) RETURNING *;

-- name: GetEnvironment :one
SELECT * FROM environments WHERE id = $1;

-- name: ListEnvironments :many
SELECT * FROM environments WHERE project_id = $1 ORDER BY created_at;

-- name: DeleteEnvironment :exec
DELETE FROM environments WHERE id = $1;

-- name: ListEnvironmentsByProjectIDs :many
-- Batched form for the topology view, which needs every environment of an
-- organization at once (one query instead of one per project).
SELECT * FROM environments WHERE project_id = ANY($1::bigint[]) ORDER BY created_at;
