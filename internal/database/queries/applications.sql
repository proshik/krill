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
