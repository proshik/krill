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
SELECT d.id, d.application_id, d.status, d.trigger, d.image_tag, d.error_message, d.started_at, d.finished_at, d.rollback_source_id,
EXISTS (SELECT 1 FROM deployment_releases r WHERE r.deployment_id=d.id AND r.confirmed AND d.status='done') AS rollback_eligible
FROM deployments d WHERE d.application_id = $1 ORDER BY d.started_at DESC LIMIT 50;

-- name: FinishDeployment :exec
UPDATE deployments
SET status = $2, image_tag = $3, error_message = $4, log = $5, finished_at = now()
WHERE id = $1;

-- name: SetDeploymentVersion :exec
-- Records the version a deployment ships, once the deployer knows it (after
-- the build or the image's digest lookup).
UPDATE deployments SET version = $2 WHERE id = $1;

-- name: GetDeploymentVersion :one
SELECT COALESCE(version, '')::text AS version FROM deployments WHERE id = $1;

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
    finished_at = now(),
    -- A deploy that recorded what it applied but died before writing done.
    applied_config = NULL
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

-- name: CountRunningDeployments :one
-- In-flight deployments across the whole instance. The self-updater refuses to
-- restart Krill while any of them runs, since the restart fails them.
SELECT count(*) FROM deployments WHERE status = 'running';

-- name: ListDeploymentStatuses :many
-- Status of each listed deployment, without the log column. The startup
-- network migration polls this until every app redeploy it submitted is
-- terminal: submitting a deploy proves nothing about whether it moved the app.
SELECT id, status FROM deployments WHERE id = ANY(@ids::bigint[]);

-- name: ListDeployedApplicationIDs :many
-- The apps that have been deployed at least once: an app with no Swarm service
-- is "not deployed" in the UI only when it has none (a stopped global app has
-- no service either, see docker ServiceScale).
SELECT DISTINCT application_id FROM deployments WHERE application_id = ANY(@ids::bigint[]);

-- name: FinishDeploymentApplied :exec
-- FinishDeployment for a deploy that ended done, with what it applied: one
-- update, so no row reads done without it or failed with it.
UPDATE deployments
SET status = @status, image_tag = @image_tag, error_message = @error_message, log = @log,
    finished_at = now(), applied_config = @applied_config
WHERE id = @id;

-- name: SetDeploymentAppliedConfig :exec
-- The configuration fingerprint a successful deployment applied.
UPDATE deployments SET applied_config = @applied_config WHERE id = @id;

-- name: GetDeploymentAppliedConfig :one
SELECT applied_config FROM deployments WHERE id = $1;

-- name: ListLatestAppliedDeployments :many
-- The newest successful deployment of each listed app, with the configuration
-- fingerprint it applied (NULL before it was recorded) and whether a deploy of
-- the app is in flight (queued deploys have a running row too).
-- One ordered index lookup per app (deployments_app_idx, stopping at the first
-- done row) and the partial running index, so the cost does not grow with the
-- history the deployments poll keeps asking about.
SELECT a.id::bigint AS application_id, d.id, d.image_tag, d.applied_config,
    EXISTS (SELECT 1 FROM deployments r WHERE r.application_id = a.id AND r.status = 'running') AS in_flight
FROM unnest(@ids::bigint[]) AS a(id)
JOIN LATERAL (
    SELECT dd.id, dd.image_tag, dd.applied_config FROM deployments dd
    WHERE dd.application_id = a.id AND dd.status = 'done'
    ORDER BY dd.started_at DESC, dd.id DESC
    LIMIT 1
) d ON true;

-- name: GetDeploymentStatus :one
-- The status alone, for a caller waiting on a deployment (the log can reach
-- ~256 KB, too much to read once a second).
SELECT status FROM deployments WHERE id = $1;
