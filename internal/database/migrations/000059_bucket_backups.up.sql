-- Scheduled snapshots of a bucket into backup storage. Each run copies every
-- object under <prefix>/<server>/<bucket>/<timestamp>/ and writes a manifest
-- (key, size, sha256) last; a restore goes into a new, empty bucket and checks
-- every object against the manifest. New table only.
CREATE TABLE bucket_backups (
    id             BIGSERIAL PRIMARY KEY,
    bucket_id      BIGINT NOT NULL REFERENCES s3_buckets(id) ON DELETE CASCADE,
    destination_id BIGINT NOT NULL REFERENCES destinations(id) ON DELETE RESTRICT,
    schedule       TEXT NOT NULL,
    prefix         TEXT NOT NULL DEFAULT '',
    retention      INTEGER NOT NULL DEFAULT 7 CHECK (retention >= 1),
    enabled        BOOLEAN NOT NULL DEFAULT true,
    last_run_at    TIMESTAMPTZ,
    last_status    TEXT NOT NULL DEFAULT '',
    last_error     TEXT NOT NULL DEFAULT '',
    last_restore_at     TIMESTAMPTZ,
    last_restore_status TEXT NOT NULL DEFAULT '',
    last_restore_error  TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX bucket_backups_bucket_id_idx ON bucket_backups (bucket_id);
CREATE INDEX bucket_backups_destination_id_idx ON bucket_backups (destination_id);
