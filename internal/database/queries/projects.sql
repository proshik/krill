-- name: CreateProject :one
INSERT INTO projects (organization_id, name, slug, description)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1;

-- name: ListProjects :many
SELECT * FROM projects WHERE organization_id = $1 ORDER BY created_at;

-- name: ListProjectsWithCounts :many
-- One aggregate query for the org dashboard (replaces a 2N+1 per-project count
-- loop). DISTINCT on environments because the app join multiplies env rows.
SELECT p.id, p.organization_id, p.name, p.slug, p.description, p.created_at,
       count(DISTINCT e.id) AS env_count,
       count(a.id)          AS app_count
FROM projects p
LEFT JOIN environments e ON e.project_id = p.id
LEFT JOIN applications a ON a.environment_id = e.id
WHERE p.organization_id = $1
GROUP BY p.id
ORDER BY p.created_at;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = $1;

-- name: CountEnvironments :one
SELECT count(*) FROM environments WHERE project_id = $1;
