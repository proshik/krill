-- name: CreateUser :one
INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at;

-- name: GetUserIsAdmin :one
SELECT is_admin FROM users WHERE id = $1;

-- name: SetUserAdmin :exec
UPDATE users SET is_admin = $2 WHERE id = $1;
