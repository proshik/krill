-- name: CreateDBLink :one
INSERT INTO app_db_links (application_id, logical_database_id, instance_id, var_name, scheme)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, application_id, logical_database_id, instance_id, var_name, scheme, created_at;

-- name: GetDBLink :one
SELECT id, application_id, logical_database_id, instance_id, var_name, scheme, field, created_at
FROM app_db_links WHERE id = $1;

-- name: ListDBLinksByApplication :many
SELECT id, application_id, logical_database_id, instance_id, var_name, scheme, field, created_at
FROM app_db_links WHERE application_id = $1 ORDER BY var_name;

-- name: DeleteDBLink :exec
DELETE FROM app_db_links WHERE id = $1;
