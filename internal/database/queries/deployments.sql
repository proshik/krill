-- name: CreateDeployment :one
INSERT INTO deployments (application_id, trigger) VALUES ($1, $2) RETURNING *;

-- name: GetDeployment :one
SELECT * FROM deployments WHERE id = $1;

-- name: ListDeploymentsByApplication :many
SELECT * FROM deployments WHERE application_id = $1 ORDER BY started_at DESC LIMIT 50;

-- name: ListDeploymentSummariesByApplication :many
-- Same rows as ListDeploymentsByApplication but without the (up to ~256KB) log
-- column — for the deploy-history list, which is polled every 2s and never
-- renders the log. Use GetDeployment for the single-deployment detail/log view.
SELECT id, application_id, status, trigger, image_tag, error_message, started_at, finished_at
FROM deployments WHERE application_id = $1 ORDER BY started_at DESC LIMIT 50;

-- name: FinishDeployment :exec
UPDATE deployments
SET status = $2, image_tag = $3, error_message = $4, log = $5, finished_at = now()
WHERE id = $1;

-- name: ClearOldDeploymentLogs :exec
UPDATE deployments SET log = ''
WHERE finished_at IS NOT NULL AND finished_at < now() - interval '1 hour' AND log <> '';
