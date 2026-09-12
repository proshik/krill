-- name: CreateUser :one
INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING *;

-- name: CreateInvitedUser :one
-- A user created by someone else's invitation, flagged to change the password
-- the inviter chose in the same statement. A separate UPDATE could fail after
-- the insert succeeded, leaving the invitee on a password the inviter still
-- knows, with nothing to ever retry it.
INSERT INTO users (email, password_hash, must_change_password) VALUES ($1, $2, true) RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserIsAdmin :one
SELECT is_admin FROM users WHERE id = $1;

-- name: SetUserAdmin :exec
UPDATE users SET is_admin = $2 WHERE id = $1;

-- name: DemoteInstanceAdminsExcept :execrows
-- Revoke the instance-operator flag from everyone except the seeded admin.
-- Keeps KRILL_ADMIN_EMAIL authoritative: rotating it must hand the role over,
-- not hand out a second one.
UPDATE users SET is_admin = false WHERE is_admin = true AND id <> $1;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = $2, must_change_password = false WHERE id = $1;

-- name: SetUserMustChangePassword :exec
UPDATE users SET must_change_password = $2 WHERE id = $1;

-- name: GetUserMustChangePassword :one
SELECT must_change_password FROM users WHERE id = $1;
