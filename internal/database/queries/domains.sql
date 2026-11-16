-- name: ListDomainsByApplication :many
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips, redirect_to_primary
FROM domains WHERE application_id = $1 ORDER BY is_primary DESC, created_at;

-- name: GetDomain :one
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips, redirect_to_primary
FROM domains WHERE id = $1;

-- name: CreateDomain :one
INSERT INTO domains (application_id, host, tls, is_primary, exposed, paths)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips, redirect_to_primary;

-- name: SetDomainTLS :exec
UPDATE domains SET tls = $2 WHERE id = $1;

-- name: DeleteDomain :exec
DELETE FROM domains WHERE id = $1;

-- name: DeleteNonPrimaryDomain :execrows
-- The primary can move between the handler's read and this write (Make
-- primary); the row lock makes the condition see the committed value.
DELETE FROM domains WHERE id = $1 AND NOT is_primary;

-- name: CountDomainsByApplication :one
SELECT count(*) FROM domains WHERE application_id = $1;

-- name: CountDomainsByHost :one
SELECT count(*) FROM domains WHERE host = $1;

-- name: UpdateDomainExposure :exec
UPDATE domains SET exposed = $2, paths = $3 WHERE id = $1;

-- name: CountExposedDomainsByApplication :one
-- A redirecting domain never serves the app, whatever its exposed flag says.
SELECT count(*) FROM domains WHERE application_id = $1 AND exposed = true AND NOT redirect_to_primary;

-- name: SetDomainBasicAuth :exec
UPDATE domains SET basic_auth_users = $2 WHERE id = $1;

-- name: SetDomainAllowedIPs :exec
UPDATE domains SET allowed_ips = $2 WHERE id = $1;

-- name: ListDomainsByApplicationIDs :many
-- Batched form for the topology view (one query instead of one per app).
SELECT id, application_id, host, tls, is_primary, created_at, exposed, paths, basic_auth_users, allowed_ips, redirect_to_primary
FROM domains WHERE application_id = ANY($1::bigint[]) ORDER BY application_id, is_primary DESC, created_at;

-- name: ListDomainClaimsByHost :many
-- Every row claiming a host, locked until its checked update commits.
SELECT d.id, d.application_id, d.paths, d.tls, d.is_primary, d.redirect_to_primary,
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

-- name: SetDomainRedirect :execrows
-- Never on the primary: the CHECK backs this up, the condition keeps it a
-- clean no-op (0 rows) instead of a constraint error.
UPDATE domains SET redirect_to_primary = @redirect::boolean
WHERE id = @id AND (NOT @redirect::boolean OR NOT is_primary);

-- name: SetPrimaryDomainRow :exec
-- Exactly one primary per application: every row of the app is rewritten.
UPDATE domains SET is_primary = (id = $2) WHERE application_id = $1;
