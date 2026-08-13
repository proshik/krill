-- name: CreateAPIToken :one
INSERT INTO api_tokens (user_id, org_id, name, token_hash, prefix, level, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListAPITokensByPrefix :many
SELECT * FROM api_tokens WHERE prefix = $1;

-- name: ListAPITokensByUser :many
SELECT * FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC;

-- name: ListAPITokensByUserAndOrg :many
SELECT * FROM api_tokens WHERE user_id = $1 AND org_id = $2 ORDER BY created_at DESC;

-- name: GetAPIToken :one
SELECT * FROM api_tokens WHERE id = $1;

-- name: DeleteAPIToken :exec
DELETE FROM api_tokens WHERE id = $1 AND user_id = $2;

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = $1;
