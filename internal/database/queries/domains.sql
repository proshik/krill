-- name: ListDomainsByApplication :many
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips
FROM domains WHERE application_id = $1 ORDER BY is_primary DESC, created_at;

-- name: GetDomain :one
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips
FROM domains WHERE id = $1;

-- name: CreateDomain :one
INSERT INTO domains (application_id, host, tls, is_primary, exposed, paths)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips;

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

-- name: SetDomainBasicAuth :exec
UPDATE domains SET basic_auth_users = $2 WHERE id = $1;

-- name: SetDomainAllowedIPs :exec
UPDATE domains SET allowed_ips = $2 WHERE id = $1;

-- name: ListDomainsByApplicationIDs :many
-- Batched form for the topology view (one query instead of one per app).
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips
FROM domains WHERE application_id = ANY($1::bigint[]) ORDER BY application_id, is_primary DESC, created_at;

-- name: ListDomainClaimsByHost :many
-- Every row claiming a host, locked until its checked update commits.
SELECT d.id, d.application_id, d.paths, d.tls,
       a.name AS app_name, a.port AS app_port, a.environment_id
FROM domains d JOIN applications a ON a.id = d.application_id
WHERE d.host = $1
ORDER BY d.id
FOR UPDATE OF d;

-- name: SetDomainTLSByHost :exec
-- TLS is a property of the host: every row sharing it switches together.
UPDATE domains SET tls = $2 WHERE host = $1;

-- name: ListHostSharesByApplication :many
-- For each domain of an app, the other apps of the same environment on its host.
SELECT mine.id AS domain_id, a.id AS app_id, a.name AS app_name, other.paths
FROM domains mine
JOIN applications me ON me.id = mine.application_id
JOIN domains other ON other.host = mine.host AND other.application_id <> mine.application_id
JOIN applications a ON a.id = other.application_id AND a.environment_id = me.environment_id
WHERE mine.application_id = $1
ORDER BY mine.id, a.name;
