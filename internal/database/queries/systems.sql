-- name: CreateSystem :one
INSERT INTO systems (environment_id, name) VALUES ($1, $2) RETURNING *;

-- name: GetSystem :one
SELECT * FROM systems WHERE id = $1;

-- name: ListSystemsByEnvironment :many
SELECT * FROM systems WHERE environment_id = $1 ORDER BY lower(name), id;

-- name: RenameSystem :exec
UPDATE systems SET name = $2 WHERE id = $1;

-- name: DeleteSystem :exec
DELETE FROM systems WHERE id = $1;

-- name: ListSystemMembersByEnvironment :many
SELECT m.application_id, m.system_id
FROM system_members m JOIN systems s ON s.id = m.system_id
WHERE s.environment_id = $1;

-- name: SetAppSystem :exec
INSERT INTO system_members (application_id, system_id) VALUES ($1, $2)
ON CONFLICT (application_id) DO UPDATE SET system_id = EXCLUDED.system_id;

-- name: RemoveAppFromSystem :exec
DELETE FROM system_members WHERE application_id = $1;

-- name: CountResourcesByInstance :one
-- Everything a server hosts that Krill models: logical databases and buckets
-- (org-wide, across environments), for the "shared" badge.
SELECT (SELECT count(*) FROM logical_databases ld WHERE ld.instance_id = $1)
     + (SELECT count(*) FROM s3_buckets b WHERE b.instance_id = $1) AS total;
