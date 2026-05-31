-- name: CreateProject :one
INSERT INTO projects (organization_id, name, slug, description)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1;

-- name: ListProjects :many
SELECT * FROM projects WHERE organization_id = $1 ORDER BY created_at;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = $1;

-- name: CountEnvironments :one
SELECT count(*) FROM environments WHERE project_id = $1;
