-- Buckets on a managed S3 instance (engine minio), scoped to an environment
-- like logical databases, each with a restricted principal Krill hands to
-- linked apps instead of the instance's root credentials. New tables only: a
-- release before this one ignores them.
CREATE TABLE s3_buckets (
    id BIGSERIAL PRIMARY KEY,
    instance_id BIGINT NOT NULL REFERENCES db_instances(id) ON DELETE RESTRICT,
    environment_id BIGINT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    -- The principal linked apps get. secret_key is secret.Enc.
    access_key TEXT NOT NULL,
    secret_key TEXT NOT NULL,
    -- The principal a rotation replaced, kept working until it is revoked.
    prev_access_key TEXT NOT NULL DEFAULT '',
    prev_secret_key TEXT NOT NULL DEFAULT '',
    -- false: an imported bucket whose data Krill must never destroy on its own.
    created_by_krill BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (instance_id, name)
);
CREATE INDEX s3_buckets_environment_id_idx ON s3_buckets (environment_id);

CREATE TABLE app_bucket_links (
    id BIGSERIAL PRIMARY KEY,
    application_id BIGINT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    -- NO ACTION (checked at the end of the statement), so deleting an
    -- environment removes its apps' links and its buckets together.
    bucket_id BIGINT NOT NULL REFERENCES s3_buckets(id),
    var_name TEXT NOT NULL,
    field TEXT NOT NULL CHECK (field IN ('endpoint', 'bucket', 'access_key', 'secret_key', 'region')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, var_name)
);
CREATE INDEX app_bucket_links_bucket_id_idx ON app_bucket_links (bucket_id);
