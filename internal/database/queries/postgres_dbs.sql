-- name: CreatePostgres :one
INSERT INTO postgres_dbs (environment_id, name, app_name, database_name, database_user, database_password, image, external_port)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: GetPostgres :one
SELECT * FROM postgres_dbs WHERE id = $1;

-- name: ListPostgresByEnvironment :many
SELECT * FROM postgres_dbs WHERE environment_id = $1 ORDER BY created_at;

-- name: UpdatePostgresStatus :exec
UPDATE postgres_dbs SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdatePostgresImage :exec
UPDATE postgres_dbs SET image = $2, updated_at = now() WHERE id = $1;

-- name: DeletePostgres :exec
DELETE FROM postgres_dbs WHERE id = $1;

-- name: GetPostgresChain :one
SELECT pd.id AS db_id, e.id AS env_id, p.id AS project_id, o.id AS org_id
FROM postgres_dbs pd
JOIN environments e ON e.id = pd.environment_id
JOIN projects p ON p.id = e.project_id
JOIN organizations o ON o.id = p.organization_id
WHERE pd.id = $1;

-- name: CountPostgresByExternalPort :one
SELECT count(*) FROM postgres_dbs WHERE external_port = $1;

-- name: ListAllPostgres :many
SELECT id, name, app_name FROM postgres_dbs;
