-- name: ListNodeLabels :many
SELECT swarm_node_id, label FROM node_labels;

-- name: UpsertNodeLabel :exec
INSERT INTO node_labels (swarm_node_id, label)
VALUES ($1, $2)
ON CONFLICT (swarm_node_id) DO UPDATE SET label = EXCLUDED.label;

-- name: DeleteNodeLabel :exec
DELETE FROM node_labels WHERE swarm_node_id = $1;
