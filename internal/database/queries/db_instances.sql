-- name: CreateDBInstance :one
INSERT INTO db_instances (organization_id, engine, name, app_name, image, superuser, superuser_password, external_port, node_hostname, console_external_port)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

-- name: GetDBInstance :one
SELECT * FROM db_instances WHERE id = $1;

-- name: ListDBInstancesByOrg :many
SELECT * FROM db_instances WHERE organization_id = $1 ORDER BY created_at;

-- name: UpdateDBInstanceStatus :exec
UPDATE db_instances SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateDBInstanceImage :exec
UPDATE db_instances SET image = $2, updated_at = now() WHERE id = $1;

-- name: SetDBInstanceNode :exec
UPDATE db_instances SET node_hostname = $2, updated_at = now() WHERE id = $1;

-- name: DeleteDBInstance :exec
DELETE FROM db_instances WHERE id = $1;

-- name: CountDBInstancesByExternalPort :one
-- Checks BOTH host-published columns: external_port and console_external_port
-- share the same host port namespace (both are socat-proxied on the manager),
-- so a candidate port must not collide with either.
SELECT count(*) FROM db_instances WHERE external_port = $1 OR console_external_port = $1;

-- name: CountLogicalDatabasesByInstance :one
SELECT count(*) FROM logical_databases WHERE instance_id = $1;

-- name: ListDBInstancesByNodeHostname :many
SELECT id, organization_id, name, engine, node_hostname FROM db_instances WHERE node_hostname = $1 ORDER BY name;

-- name: UpdateDBInstanceExternalPort :exec
UPDATE db_instances SET external_port = $2 WHERE id = $1;

-- name: UpdateDBInstanceConsolePort :exec
UPDATE db_instances SET console_external_port = $2 WHERE id = $1;

-- name: CountOtherDBInstancesByExternalPort :one
-- Same both-columns check as CountDBInstancesByExternalPort, excluding the
-- instance's own row (an edit must not conflict with itself).
SELECT count(*) FROM db_instances WHERE (external_port = $1 OR console_external_port = $1) AND id <> $2;
