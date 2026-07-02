-- name: CreateLogicalDatabase :one
INSERT INTO logical_databases (instance_id, environment_id, name, db_name, username, password)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetLogicalDatabase :one
SELECT * FROM logical_databases WHERE id = $1;

-- name: ListLogicalDatabasesByEnvironment :many
SELECT ld.*, di.name AS instance_name, di.app_name AS instance_app_name,
       di.status AS instance_status, di.image AS instance_image,
       di.node_hostname AS instance_node, di.external_port AS instance_external_port
FROM logical_databases ld
JOIN db_instances di ON di.id = ld.instance_id
WHERE ld.environment_id = $1 ORDER BY ld.created_at;

-- name: ListLogicalDatabasesByInstance :many
SELECT ld.*, e.name AS env_name, p.name AS project_name, p.id AS project_id
FROM logical_databases ld
JOIN environments e ON e.id = ld.environment_id
JOIN projects p     ON p.id = e.project_id
WHERE ld.instance_id = $1 ORDER BY ld.created_at;

-- name: DeleteLogicalDatabase :exec
DELETE FROM logical_databases WHERE id = $1;
