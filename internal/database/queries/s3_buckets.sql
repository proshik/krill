-- name: CreateS3Bucket :one
INSERT INTO s3_buckets (instance_id, environment_id, name, access_key, secret_key, created_by_krill, access_key_generated)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetS3Bucket :one
SELECT * FROM s3_buckets WHERE id = $1;

-- name: ListS3BucketsByEnvironment :many
SELECT b.*, di.name AS instance_name, di.app_name AS instance_app_name, di.status AS instance_status
FROM s3_buckets b
JOIN db_instances di ON di.id = b.instance_id
WHERE b.environment_id = $1 ORDER BY b.created_at;

-- name: ListS3BucketsByInstance :many
SELECT * FROM s3_buckets WHERE instance_id = $1 ORDER BY created_at;

-- name: CountS3BucketsByInstance :one
SELECT count(*) FROM s3_buckets WHERE instance_id = $1;

-- name: RotateS3BucketKey :execrows
-- A rotation always installs a generated principal. It applies only while no
-- previous key is pending, so of two concurrent rotations one wins.
UPDATE s3_buckets
SET prev_access_key = access_key, prev_secret_key = secret_key,
    prev_access_key_generated = access_key_generated,
    access_key = $2, secret_key = $3, access_key_generated = true
WHERE id = $1 AND prev_access_key = '';

-- name: ClearS3BucketPrevKey :exec
UPDATE s3_buckets SET prev_access_key = '', prev_secret_key = '', prev_access_key_generated = false WHERE id = $1;

-- name: DeleteS3Bucket :exec
DELETE FROM s3_buckets WHERE id = $1;

-- name: CreateBucketLink :one
INSERT INTO app_bucket_links (application_id, bucket_id, var_name, field)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetBucketLink :one
SELECT * FROM app_bucket_links WHERE id = $1;

-- name: ListBucketLinksByApplication :many
SELECT * FROM app_bucket_links WHERE application_id = $1 ORDER BY var_name;

-- name: ListBucketLinksByApplicationIDs :many
SELECT * FROM app_bucket_links WHERE application_id = ANY($1::bigint[]) ORDER BY application_id, var_name;

-- name: ListBucketLinksByBucket :many
SELECT l.*, a.name AS app_name
FROM app_bucket_links l JOIN applications a ON a.id = l.application_id
WHERE l.bucket_id = $1 ORDER BY a.name, l.var_name;

-- name: DeleteBucketLink :exec
DELETE FROM app_bucket_links WHERE id = $1;
