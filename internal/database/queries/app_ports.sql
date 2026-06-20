-- name: CreateAppPort :one
INSERT INTO app_ports (application_id, host_port, container_port, protocol)
VALUES ($1, $2, $3, $4)
RETURNING id, application_id, host_port, container_port, protocol;

-- name: GetAppPort :one
SELECT id, application_id, host_port, container_port, protocol
FROM app_ports WHERE id = $1;

-- name: ListAppPorts :many
SELECT id, application_id, host_port, container_port, protocol
FROM app_ports WHERE application_id = $1 ORDER BY host_port, protocol;

-- name: DeleteAppPort :exec
DELETE FROM app_ports WHERE id = $1 AND application_id = $2;

-- name: CountAppPortsByHostPort :one
SELECT count(*) FROM app_ports WHERE host_port = $1 AND protocol = $2;
