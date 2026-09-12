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

-- name: FailOrphanedDeployments :execrows
-- Reconcile deploys that were in flight when the process died: nothing will
-- ever finish them, so they read as permanently running in the history. Safe to
-- run at startup only — a just-booted control plane has no deploy in flight.
UPDATE deployments
SET status = 'error',
    error_message = 'interrupted: krill restarted while this deploy was running',
    finished_at = now()
WHERE status = 'running';

-- name: CountRunningDeploymentsByApplication :one
-- How many deploys for this app are still in flight. Used to refuse a second
-- one: the queue is 64 deep, single-worker and shared by every tenant, so a
-- caller retrying the same app in a loop could otherwise fill it and make
-- every other tenant's deploys fail with "queue full". Reading committed rows
-- rather than an in-memory lock keeps this self-healing — a deploy interrupted
-- by a crash is reconciled by FailOrphanedDeployments at startup, whereas a
-- leaked lock would block the app forever.
SELECT count(*) FROM deployments WHERE application_id = $1 AND status = 'running';

-- name: CountRunningDeploymentsByOrg :one
-- In-flight deployments across the whole organization that owns $1. One tenant
-- must not be able to fill the single build worker's queue for everyone else.
SELECT count(*)
FROM deployments d
JOIN applications a ON a.id = d.application_id
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
WHERE d.status = 'running'
  AND p.organization_id = (
    SELECT p2.organization_id
    FROM applications a2
    JOIN environments e2 ON e2.id = a2.environment_id
    JOIN projects p2 ON p2.id = e2.project_id
    WHERE a2.id = $1
  );

-- name: ListDeploymentStatuses :many
-- Status of each listed deployment, without the log column. The startup
-- network migration polls this until every app redeploy it submitted is
-- terminal: submitting a deploy proves nothing about whether it moved the app.
SELECT id, status FROM deployments WHERE id = ANY(@ids::bigint[]);
