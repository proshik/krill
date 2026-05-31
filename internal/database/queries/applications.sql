-- name: CreateApplication :one
INSERT INTO applications (environment_id, name, image, tag, domain, port, env)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetApplication :one
SELECT * FROM applications WHERE id = $1;

-- name: ListApplicationsByEnvironment :many
SELECT * FROM applications WHERE environment_id = $1 ORDER BY created_at DESC;

-- name: UpdateApplicationImage :exec
UPDATE applications SET image = $2, tag = $3, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationEnv :exec
UPDATE applications SET env = $2, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationStatus :exec
UPDATE applications SET status = $2, updated_at = now() WHERE id = $1;

-- name: ListApplicationsByEnvironmentIDs :many
SELECT * FROM applications WHERE environment_id = ANY($1::bigint[]) ORDER BY created_at DESC;

-- name: GetApplicationChain :one
SELECT a.id AS app_id, e.id AS env_id, p.id AS project_id, o.id AS org_id
FROM applications a
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
JOIN organizations o ON o.id = p.organization_id
WHERE a.id = $1;

-- name: DeleteApplication :exec
DELETE FROM applications WHERE id = $1;
