-- name: CreateApplication :one
INSERT INTO applications (environment_id, name, image, tag, domain, port, env_text, source_type, git_url, git_branch, dockerfile_path)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING *;

-- name: GetApplication :one
SELECT * FROM applications WHERE id = $1;

-- name: ListApplicationsByEnvironment :many
SELECT * FROM applications WHERE environment_id = $1 ORDER BY created_at DESC;

-- name: UpdateApplicationImage :exec
UPDATE applications SET image = $2, tag = $3, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationEnv :exec
UPDATE applications SET env_text = $2, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationStatus :exec
UPDATE applications SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationSource :exec
UPDATE applications SET git_url = $2, git_branch = $3, dockerfile_path = $4, updated_at = now() WHERE id = $1;

-- name: ListApplicationsByEnvironmentIDs :many
SELECT * FROM applications WHERE environment_id = ANY($1::bigint[]) ORDER BY created_at DESC;

-- name: ListAppIndexByEnvironmentIDs :many
-- What the organization-wide index (palette, used-by lists, nodes,
-- monitoring links) needs of each app; never its env or build values.
SELECT id, name, environment_id, status, registry_id, git_credential_id
FROM applications WHERE environment_id = ANY($1::bigint[]) ORDER BY created_at DESC;

-- name: GetApplicationChain :one
SELECT a.id AS app_id, e.id AS env_id, p.id AS project_id, o.id AS org_id
FROM applications a
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
JOIN organizations o ON o.id = p.organization_id
WHERE a.id = $1;

-- name: DeleteApplication :exec
DELETE FROM applications WHERE id = $1;

-- name: SetApplicationRegistry :exec
UPDATE applications SET registry_id = $2, updated_at = now() WHERE id = $1;

-- name: SetApplicationGitCredential :exec
UPDATE applications SET git_credential_id = $2, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationBuild :exec
UPDATE applications SET build_args = $2, build_secrets = $3, updated_at = now() WHERE id = $1;

-- name: SetApplicationPlacement :exec
UPDATE applications SET placement_mode = $2, placement_nodes = $3, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationAdvanced :exec
UPDATE applications SET
    command = $2,
    memory_limit = $3,
    cpu_limit = $4,
    replicas = $5,
    restart_condition = $6,
    restart_max_attempts = $7,
    healthcheck_cmd = $8,
    healthcheck_interval = $9,
    healthcheck_timeout = $10,
    healthcheck_retries = $11,
    healthcheck_start_period = $12,
    predeploy_command = $13,
    updated_at = now()
WHERE id = $1;

-- name: CountApplicationsByProject :one
SELECT count(*) FROM applications a
JOIN environments e ON e.id = a.environment_id
WHERE e.project_id = $1;

-- name: SetApplicationAutoDeploy :exec
UPDATE applications SET auto_deploy = $2, updated_at = now() WHERE id = $1;

-- name: SetApplicationWebhookSecret :exec
UPDATE applications SET webhook_secret = $2, updated_at = now() WHERE id = $1;

-- name: ListPinnedApplications :many
SELECT id, name, placement_mode, placement_nodes FROM applications
WHERE placement_mode IN ('pin','global') AND placement_nodes <> '';

-- name: ListApplicationIDsByOrg :many
SELECT a.id
FROM applications a
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
WHERE p.organization_id = $1
ORDER BY a.id;

-- name: GetApplicationIdentity :one
-- The names and ids an app's containers are labelled with for log collection.
SELECT o.id AS org_id, o.name AS org_name, p.name AS project_name, e.name AS env_name
FROM applications a
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
JOIN organizations o ON o.id = p.organization_id
WHERE a.id = $1;

-- name: UpdateApplicationEnvClassified :exec
UPDATE applications SET env_text = $2, env_public_keys = $3, updated_at = now() WHERE id = $1;

-- name: GetApplicationPlacement :one
SELECT placement_mode, placement_nodes FROM applications WHERE id = $1;

-- name: SetApplicationDomain :exec
UPDATE applications SET domain = $2, updated_at = now() WHERE id = $1;

-- name: LockApplicationRow :one
-- Taken before any domains row: deleting an application locks it first and
-- then its domains (ON DELETE CASCADE), so the opposite order could deadlock.
SELECT id FROM applications WHERE id = $1 FOR UPDATE;

-- name: ListStuckDeployingApplications :many
-- Apps whose stored status says a deploy is under way while no deployment of
-- theirs is: a deploy interrupted by a shutdown or a crash leaves the status
-- behind, and nothing else ever rewrites it. last_finished is when the app's
-- latest deployment ended, or its row last changed when it has none;
-- last_status is that deployment's status ('' when none); updated_at lets the
-- write tell whether anything touched the row since.
SELECT a.id, a.updated_at,
       COALESCE((SELECT max(d.finished_at) FROM deployments d WHERE d.application_id = a.id), a.updated_at)::timestamptz AS last_finished,
       COALESCE((SELECT d.status FROM deployments d WHERE d.application_id = a.id ORDER BY d.id DESC LIMIT 1), '')::text AS last_status
FROM applications a
WHERE a.status = 'deploying'
  AND NOT EXISTS (SELECT 1 FROM deployments d WHERE d.application_id = a.id AND d.status = 'running');

-- name: HealApplicationStatus :execrows
-- Rewrites a stuck 'deploying' status, but only while the row is exactly as
-- listed: every status write sets updated_at, so a deploy that started (or
-- started and ended) since the listing keeps what it wrote — including one the
-- statement's snapshot of deployments cannot see yet.
UPDATE applications a SET status = @status, updated_at = now()
WHERE a.id = @id AND a.status = 'deploying' AND a.updated_at = @seen
  AND NOT EXISTS (SELECT 1 FROM deployments d WHERE d.application_id = a.id AND d.status = 'running');
