-- name: CreateMember :one
INSERT INTO members (organization_id, user_id, role) VALUES ($1, $2, $3) RETURNING *;

-- name: GetMembership :one
SELECT * FROM members WHERE organization_id = $1 AND user_id = $2;

-- name: GetMemberByID :one
SELECT * FROM members WHERE id = $1;

-- name: ListMembers :many
SELECT m.id, m.organization_id, m.user_id, m.role, m.created_at, u.email
FROM members m JOIN users u ON u.id = m.user_id
WHERE m.organization_id = $1
ORDER BY m.created_at;

-- name: UpdateMemberRole :exec
UPDATE members SET role = $2 WHERE id = $1;

-- name: DeleteMember :exec
DELETE FROM members WHERE id = $1;

-- name: CountOwners :one
SELECT count(*) FROM members WHERE organization_id = $1 AND role = 'owner';
