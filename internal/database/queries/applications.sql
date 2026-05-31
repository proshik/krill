-- name: CreateApplication :one
INSERT INTO applications (name, image, tag, domain, port, env)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetApplication :one
SELECT * FROM applications WHERE id = $1;

-- name: GetApplicationByName :one
SELECT * FROM applications WHERE name = $1;

-- name: ListApplications :many
SELECT * FROM applications ORDER BY created_at DESC;

-- name: UpdateApplicationImage :exec
UPDATE applications SET image = $2, tag = $3, updated_at = now() WHERE id = $1;

-- name: UpdateApplicationStatus :exec
UPDATE applications SET status = $2, updated_at = now() WHERE id = $1;
