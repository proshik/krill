CREATE TABLE app_volumes (
    id             BIGSERIAL PRIMARY KEY,
    application_id BIGINT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    mount_path     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The composite UNIQUE(application_id, name) also serves as the left-prefix
-- index for application_id lookups + the FK, so no separate app_id index.
CREATE UNIQUE INDEX app_volumes_app_name_key ON app_volumes(application_id, name);
CREATE UNIQUE INDEX app_volumes_app_path_key ON app_volumes(application_id, mount_path);

-- Used in Stage B (volume backup configs). Created now so Stage B needs no new
-- migration; unused until then.
CREATE TABLE volume_backups (
    id             BIGSERIAL PRIMARY KEY,
    app_volume_id  BIGINT NOT NULL REFERENCES app_volumes(id) ON DELETE CASCADE,
    destination_id BIGINT NOT NULL REFERENCES destinations(id) ON DELETE RESTRICT,
    schedule       TEXT NOT NULL,
    prefix         TEXT NOT NULL DEFAULT '',
    retention      INTEGER NOT NULL DEFAULT 7 CHECK (retention >= 1),
    enabled        BOOLEAN NOT NULL DEFAULT true,
    last_run_at    TIMESTAMPTZ,
    last_status    TEXT NOT NULL DEFAULT '',
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX volume_backups_app_volume_id_idx ON volume_backups(app_volume_id);
CREATE INDEX volume_backups_destination_id_idx ON volume_backups(destination_id);
