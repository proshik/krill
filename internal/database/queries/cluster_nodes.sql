-- name: CreateClusterNode :one
INSERT INTO cluster_nodes (name, ssh_host, ssh_port, ssh_user, ssh_key, host_key, swarm_node_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, name, ssh_host, ssh_port, ssh_user, ssh_key, host_key, swarm_node_id, created_at;

-- name: ListClusterNodes :many
SELECT id, name, ssh_host, ssh_port, ssh_user, ssh_key, host_key, swarm_node_id, created_at
FROM cluster_nodes ORDER BY name;

-- name: GetClusterNode :one
SELECT id, name, ssh_host, ssh_port, ssh_user, ssh_key, host_key, swarm_node_id, created_at
FROM cluster_nodes WHERE id = $1;

-- name: SetClusterNodeSwarmID :exec
UPDATE cluster_nodes SET swarm_node_id = $2 WHERE id = $1;

-- name: SetClusterNodeHostKey :exec
UPDATE cluster_nodes SET host_key = $2 WHERE id = $1;

-- name: DeleteClusterNode :exec
DELETE FROM cluster_nodes WHERE id = $1;

-- name: CountClusterNodesByName :one
SELECT count(*) FROM cluster_nodes WHERE name = $1;
