-- name: CreateEnvironment :one
INSERT INTO environments (project_id, name, slug) VALUES ($1, $2, $3) RETURNING *;

-- name: GetEnvironment :one
SELECT * FROM environments WHERE id = $1;

-- name: ListEnvironments :many
SELECT * FROM environments WHERE project_id = $1 ORDER BY created_at;

-- name: DeleteEnvironment :exec
DELETE FROM environments WHERE id = $1;
