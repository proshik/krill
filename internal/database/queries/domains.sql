-- name: ListDomainsByApplication :many
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths
FROM domains WHERE application_id = $1 ORDER BY is_primary DESC, created_at;

-- name: GetDomain :one
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths
FROM domains WHERE id = $1;

-- name: CreateDomain :one
INSERT INTO domains (application_id, host, tls, is_primary, exposed, paths)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, application_id, host, tls, is_primary, created_at, exposed, paths;

-- name: SetDomainTLS :exec
UPDATE domains SET tls = $2 WHERE id = $1;

-- name: DeleteDomain :exec
DELETE FROM domains WHERE id = $1;

-- name: CountDomainsByApplication :one
SELECT count(*) FROM domains WHERE application_id = $1;

-- name: CountDomainsByHost :one
SELECT count(*) FROM domains WHERE host = $1;

-- name: UpdateDomainExposure :exec
UPDATE domains SET exposed = $2, paths = $3 WHERE id = $1;

-- name: CountExposedDomainsByApplication :one
SELECT count(*) FROM domains WHERE application_id = $1 AND exposed = true;
