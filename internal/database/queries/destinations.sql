-- name: CreateDestination :one
INSERT INTO destinations (organization_id, name, endpoint, bucket, region, access_key, secret_key)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, organization_id, name, endpoint, bucket, region, access_key, secret_key, created_at;

-- name: GetDestination :one
SELECT id, organization_id, name, endpoint, bucket, region, access_key, secret_key, created_at
FROM destinations WHERE id = $1;

-- name: ListDestinationsByOrg :many
SELECT id, organization_id, name, endpoint, bucket, region, access_key, secret_key, created_at
FROM destinations WHERE organization_id = $1 ORDER BY name;

-- name: DeleteDestination :exec
DELETE FROM destinations WHERE id = $1;

-- name: CountDestinationsByName :one
SELECT count(*) FROM destinations WHERE organization_id = $1 AND name = $2;

-- name: CountBackupsByDestination :one
SELECT count(*) FROM backups WHERE destination_id = $1;
