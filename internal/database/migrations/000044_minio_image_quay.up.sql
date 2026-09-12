-- Docker Hub removed the minio/minio repository, so every MinIO instance created
-- before this migration stores an image reference that can no longer be pulled,
-- and its next redeploy (a node move, a restart after a crash, the per-organization
-- network migration) fails. MinIO still publishes the same tags on quay.io, so the
-- repository part is rewritten and whatever tag or digest the instance used is kept.
-- Only a Docker Hub reference to exactly minio/minio is touched: a private mirror,
-- a different repository and an image already on quay.io are left alone.
UPDATE db_instances
SET image = regexp_replace(
        image,
        '^(docker\.io/|index\.docker\.io/|registry-1\.docker\.io/)?minio/minio(?=[:@]|$)',
        'quay.io/minio/minio'),
    updated_at = now()
WHERE engine = 'minio'
  AND image ~ '^(docker\.io/|index\.docker\.io/|registry-1\.docker\.io/)?minio/minio([:@]|$)';
