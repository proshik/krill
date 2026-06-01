-- name: CreateDeployment :one
INSERT INTO deployments (application_id, trigger) VALUES ($1, $2) RETURNING *;

-- name: GetDeployment :one
SELECT * FROM deployments WHERE id = $1;

-- name: ListDeploymentsByApplication :many
SELECT * FROM deployments WHERE application_id = $1 ORDER BY started_at DESC LIMIT 50;

-- name: FinishDeployment :exec
UPDATE deployments
SET status = $2, image_tag = $3, error_message = $4, log = $5, finished_at = now()
WHERE id = $1;

-- name: ClearOldDeploymentLogs :exec
UPDATE deployments SET log = ''
WHERE finished_at IS NOT NULL AND finished_at < now() - interval '1 hour' AND log <> '';
