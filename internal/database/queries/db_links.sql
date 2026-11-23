-- name: CreateDBLink :one
INSERT INTO app_db_links (application_id, logical_database_id, instance_id, var_name, scheme, field)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, application_id, logical_database_id, instance_id, var_name, scheme, field, created_at;

-- name: GetDBLink :one
SELECT id, application_id, logical_database_id, instance_id, var_name, scheme, field, created_at
FROM app_db_links WHERE id = $1;

-- name: ListDBLinksByApplication :many
SELECT id, application_id, logical_database_id, instance_id, var_name, scheme, field, created_at
FROM app_db_links WHERE application_id = $1 ORDER BY var_name;

-- name: DeleteDBLink :exec
DELETE FROM app_db_links WHERE id = $1;

-- name: ListDBLinksByApplicationIDs :many
-- Batched form for the topology view (one query instead of one per app).
SELECT id, application_id, logical_database_id, instance_id, var_name, scheme, field, created_at
FROM app_db_links WHERE application_id = ANY($1::bigint[]) ORDER BY application_id, var_name;

-- name: ListDBLinkAppNamesByLogicalDatabase :many
-- The apps a logical database is linked into: it cannot be deleted while any
-- link remains (the same rule as buckets).
SELECT DISTINCT a.name
FROM app_db_links l JOIN applications a ON a.id = l.application_id
WHERE l.logical_database_id = $1 ORDER BY a.name;

-- name: ListDBLinkAppNamesByInstance :many
-- The apps linked to a database server, directly or through one of its
-- logical databases: the server cannot be deleted while any link remains.
SELECT DISTINCT a.name
FROM app_db_links l
JOIN applications a ON a.id = l.application_id
LEFT JOIN logical_databases ld ON ld.id = l.logical_database_id
WHERE l.instance_id = @instance_id OR ld.instance_id = @instance_id
ORDER BY a.name;
