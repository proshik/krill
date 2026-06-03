CREATE TABLE destinations (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    endpoint        TEXT NOT NULL DEFAULT '',
    bucket          TEXT NOT NULL,
    region          TEXT NOT NULL DEFAULT 'us-east-1',
    access_key      TEXT NOT NULL,
    secret_key      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX destinations_org_name_key ON destinations(organization_id, name);

CREATE TABLE backups (
    id             BIGSERIAL PRIMARY KEY,
    postgres_db_id BIGINT NOT NULL REFERENCES postgres_dbs(id) ON DELETE CASCADE,
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
CREATE INDEX backups_postgres_db_id_idx ON backups(postgres_db_id);
CREATE INDEX backups_destination_id_idx ON backups(destination_id);
