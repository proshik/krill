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

-- name: CountOrganizations :one
SELECT count(*) FROM organizations;

-- name: SetOrganizationNetwork :exec
UPDATE organizations SET network_name = $2 WHERE id = $1;

-- name: ListOrganizations :many
SELECT * FROM organizations ORDER BY id;

-- name: GetOrganizationNetworkByApp :one
SELECT o.network_name
FROM applications a
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
JOIN organizations o ON o.id = p.organization_id
WHERE a.id = $1;

-- name: MarkOrganizationNetworkMigrated :exec
UPDATE organizations SET network_migrated_at = now() WHERE id = $1;
