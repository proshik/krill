-- name: InsertEnvironmentClone :exec
INSERT INTO environment_clones
(request_key, user_id, organization_id, project_id, source_id, destination_name, destination_slug, deadline)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (request_key) DO NOTHING;

-- name: GetEnvironmentClone :one
SELECT * FROM environment_clones WHERE request_key = $1;

-- name: LockEnvironmentClone :one
SELECT * FROM environment_clones WHERE request_key = $1 FOR UPDATE;

-- name: ReserveEnvironmentCloneSlug :execrows
INSERT INTO environment_clone_reservations (project_id, slug, request_key)
VALUES ($1,$2,$3) ON CONFLICT DO NOTHING;

-- name: ReleaseEnvironmentCloneSlug :exec
DELETE FROM environment_clone_reservations WHERE request_key = $1;

-- name: RecordEnvironmentCloneIntent :execrows
UPDATE environment_clones SET resource_intent = $2, updated_at = now()
WHERE request_key = $1 AND state = 'preparing' AND resource_intent = '';

-- name: SetEnvironmentCloneCleanup :execrows
UPDATE environment_clones SET state = 'cleanup', updated_at = now()
WHERE request_key = $1 AND state IN ('preparing','cleanup');

-- name: CompleteEnvironmentCloneCleanup :execrows
UPDATE environment_clones SET state = 'aborted', updated_at = now()
WHERE request_key = $1 AND state = 'cleanup';

-- name: PublishEnvironmentClone :execrows
UPDATE environment_clones SET state = 'succeeded', target_id = $2, updated_at = now()
WHERE request_key = $1 AND state = 'preparing' AND resource_intent <> '';

-- name: ListEnvironmentCloneRecovery :many
SELECT * FROM environment_clones
WHERE state IN ('preparing','cleanup') OR (NOT fences_pruned AND deadline <= now())
ORDER BY updated_at LIMIT 32;

-- name: MarkEnvironmentCloneFencesPruned :execrows
UPDATE environment_clones SET fences_pruned = true, updated_at = now()
WHERE request_key = $1 AND state IN ('succeeded','aborted') AND deadline <= now();

-- name: CountActiveEnvironmentClones :one
SELECT count(*) FROM environment_clones WHERE state IN ('preparing','cleanup');

-- name: CreateCloneApplication :one
INSERT INTO applications (id, environment_id, name, image, tag, domain, port, env_text, source_type, git_url, git_branch, dockerfile_path)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING *;

-- name: EnvironmentCloneNotice :one
SELECT EXISTS(SELECT 1 FROM environment_clones c WHERE c.target_id = $1 AND c.state = 'succeeded') AS cloned,
       EXISTS(SELECT 1 FROM environment_clones c WHERE c.source_id = $1 AND c.state IN ('preparing','cleanup')) AS pending;

-- name: TouchEnvironmentCloneRecovery :exec
UPDATE environment_clones SET updated_at = now() WHERE request_key = $1;

-- name: AdvanceEnvironmentCloneCleanup :one
UPDATE environment_clones
SET resource_intent = jsonb_set(resource_intent::jsonb, '{cleaned_databases}',
    to_jsonb(sqlc.arg(cleaned_databases)::integer))::text, updated_at = now()
WHERE request_key = sqlc.arg(request_key) AND state = 'cleanup'
  AND resource_intent = sqlc.arg(expected_intent)
RETURNING resource_intent;
