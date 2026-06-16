-- name: CreateGitCredential :one
INSERT INTO git_credentials (organization_id, name, host, username, token)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, organization_id, name, host, username, token, created_at;

-- name: GetGitCredential :one
SELECT id, organization_id, name, host, username, token, created_at
FROM git_credentials WHERE id = $1;

-- name: ListGitCredentialsByOrg :many
SELECT id, organization_id, name, host, username, token, created_at
FROM git_credentials WHERE organization_id = $1 ORDER BY name;

-- name: DeleteGitCredential :exec
DELETE FROM git_credentials WHERE id = $1;

-- name: CountGitCredentialsByName :one
SELECT count(*) FROM git_credentials WHERE organization_id = $1 AND name = $2;

-- name: CountApplicationsByGitCredential :one
SELECT count(*) FROM applications WHERE git_credential_id = $1;
