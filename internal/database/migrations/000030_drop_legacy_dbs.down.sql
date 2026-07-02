-- WARNING: destructive rollback. The legacy tables are recreated EMPTY — the
-- pre-conversion rows and the legacy id mapping are unrecoverable here.
CREATE TABLE postgres_dbs (
    id                BIGSERIAL PRIMARY KEY,
    environment_id    BIGINT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    app_name          TEXT NOT NULL UNIQUE,
    database_name     TEXT NOT NULL,
    database_user     TEXT NOT NULL,
    database_password TEXT NOT NULL,
    image             TEXT NOT NULL DEFAULT 'postgres:17',
    external_port     INTEGER,
    status            TEXT NOT NULL DEFAULT 'idle' CHECK (status IN ('idle','running','error')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    node_hostname     TEXT NOT NULL DEFAULT '',
    UNIQUE(environment_id, name)
);
CREATE TABLE redis_dbs (
    id             BIGSERIAL PRIMARY KEY,
    environment_id BIGINT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    app_name       TEXT NOT NULL UNIQUE,
    password       TEXT NOT NULL,
    image          TEXT NOT NULL DEFAULT 'redis:7',
    external_port  INTEGER,
    status         TEXT NOT NULL DEFAULT 'idle' CHECK (status IN ('idle','running','error')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    node_hostname  TEXT NOT NULL DEFAULT ''
);

ALTER TABLE db_instances ADD COLUMN legacy_pg_id BIGINT, ADD COLUMN legacy_redis_id BIGINT;
ALTER TABLE logical_databases ADD COLUMN legacy_pg_id BIGINT;

ALTER TABLE backups ADD COLUMN postgres_db_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE backups ALTER COLUMN logical_database_id DROP NOT NULL;

ALTER TABLE app_db_links DROP CONSTRAINT app_db_links_target_chk;
ALTER TABLE app_db_links
    ADD COLUMN engine TEXT NOT NULL DEFAULT '',
    ADD COLUMN db_id  BIGINT NOT NULL DEFAULT 0;
