-- name: SaveDeploymentRelease :exec
INSERT INTO deployment_releases (deployment_id, application_id, payload, source_deployment_id)
VALUES ($1, $2, $3, $4);

-- name: GetDeploymentRelease :one
SELECT r.* FROM deployment_releases r JOIN deployments d ON d.id = r.deployment_id
WHERE r.deployment_id = $1 AND r.application_id = $2 AND r.confirmed AND d.status = 'done';

-- name: ConfirmDeploymentRelease :exec
UPDATE deployment_releases SET confirmed = true WHERE deployment_id = $1;

-- name: RestoreReleaseRuntime :execrows
WITH restored_app AS (
 UPDATE applications a SET
 image = (@restored::jsonb->>'image'),
 tag = (@restored::jsonb->>'tag'),
 source_type = (@restored::jsonb->>'source_type'),
 git_url = (@restored::jsonb->>'git_url'),
 git_branch = (@restored::jsonb->>'git_branch'),
 dockerfile_path = (@restored::jsonb->>'dockerfile_path'),
 env_text = (@restored::jsonb->>'env_text'),
 env_public_keys = ARRAY(SELECT jsonb_array_elements_text(@restored::jsonb->'env_public_keys')),
 command = (@restored::jsonb->>'command'),
 port = (@restored::jsonb->>'port')::integer,
 replicas = (@restored::jsonb->>'replicas')::integer,
 memory_limit = (@restored::jsonb->>'memory_limit'),
 cpu_limit = (@restored::jsonb->>'cpu_limit'),
 restart_condition = (@restored::jsonb->>'restart_condition'),
 restart_max_attempts = (@restored::jsonb->>'restart_max_attempts')::integer,
 healthcheck_cmd = (@restored::jsonb->>'healthcheck_cmd'),
 healthcheck_interval = (@restored::jsonb->>'healthcheck_interval'),
 healthcheck_timeout = (@restored::jsonb->>'healthcheck_timeout'),
 healthcheck_retries = (@restored::jsonb->>'healthcheck_retries')::integer,
 healthcheck_start_period = (@restored::jsonb->>'healthcheck_start_period'),
 predeploy_command = (@restored::jsonb->>'predeploy_command'), updated_at = now()
 WHERE a.id = @id AND jsonb_build_object('image', a.image, 'tag', a.tag, 'source_type', a.source_type, 'git_url', a.git_url, 'git_branch', a.git_branch, 'dockerfile_path', a.dockerfile_path, 'env_text', a.env_text, 'env_public_keys', a.env_public_keys, 'command', a.command, 'port', a.port, 'replicas', a.replicas, 'memory_limit', a.memory_limit, 'cpu_limit', a.cpu_limit, 'restart_condition', a.restart_condition, 'restart_max_attempts', a.restart_max_attempts, 'healthcheck_cmd', a.healthcheck_cmd, 'healthcheck_interval', a.healthcheck_interval, 'healthcheck_timeout', a.healthcheck_timeout, 'healthcheck_retries', a.healthcheck_retries, 'healthcheck_start_period', a.healthcheck_start_period, 'predeploy_command', a.predeploy_command) = @expected::jsonb
 RETURNING a.id
)
UPDATE deployment_releases SET confirmed = true
WHERE deployment_id = @deployment_id AND application_id IN (SELECT id FROM restored_app);

-- name: SetDeploymentRollbackSource :exec
UPDATE deployments SET rollback_source_id = $2 WHERE id = $1;
