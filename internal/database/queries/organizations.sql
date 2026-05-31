-- name: CreateOrganization :one
INSERT INTO organizations (name, slug, owner_id) VALUES ($1, $2, $3) RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations WHERE id = $1;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = $1;

-- name: ListOrganizationsForUser :many
SELECT o.* FROM organizations o
JOIN members m ON m.organization_id = o.id
WHERE m.user_id = $1
ORDER BY o.created_at;

-- name: DeleteOrganization :exec
DELETE FROM organizations WHERE id = $1;
