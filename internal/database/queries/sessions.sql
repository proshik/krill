-- name: CreateSession :exec
INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3);

-- name: GetSession :one
SELECT * FROM sessions WHERE token = $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token = $1;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < now();

-- name: DeleteSessionsByUserExcept :exec
-- Changing a password logs out every other device; the current session is kept
-- so the user is not bounced back to the login form mid-flow.
DELETE FROM sessions WHERE user_id = $1 AND token <> $2;
