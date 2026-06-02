-- name: CreateRedis :one
INSERT INTO redis_dbs (environment_id, name, app_name, password, image, external_port)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetRedis :one
SELECT * FROM redis_dbs WHERE id = $1;

-- name: ListRedisByEnvironment :many
SELECT * FROM redis_dbs WHERE environment_id = $1 ORDER BY created_at;

-- name: UpdateRedisStatus :exec
UPDATE redis_dbs SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateRedisImage :exec
UPDATE redis_dbs SET image = $2, updated_at = now() WHERE id = $1;

-- name: UpdateRedisExternalPort :exec
UPDATE redis_dbs SET external_port = $2, updated_at = now() WHERE id = $1;

-- name: DeleteRedis :exec
DELETE FROM redis_dbs WHERE id = $1;

-- name: GetRedisChain :one
SELECT rd.id AS db_id, e.id AS env_id, p.id AS project_id, o.id AS org_id
FROM redis_dbs rd
JOIN environments e ON e.id = rd.environment_id
JOIN projects p ON p.id = e.project_id
JOIN organizations o ON o.id = p.organization_id
WHERE rd.id = $1;
