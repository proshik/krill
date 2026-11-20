-- Backups of Krill's own state database (internal/statebackup). Additive:
-- new tables only. One settings row for the whole instance; the storage is
-- NOT an org-scoped destination, since a dump holds every organization's data.
CREATE TABLE state_backup_settings (
    id          SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    enabled     BOOLEAN NOT NULL DEFAULT false,
    endpoint    TEXT NOT NULL DEFAULT '',   -- '' = AWS
    bucket      TEXT NOT NULL DEFAULT '',
    region      TEXT NOT NULL DEFAULT '',
    access_key  TEXT NOT NULL DEFAULT '',
    secret_key  TEXT NOT NULL DEFAULT '',   -- secret.Enc
    prefix      TEXT NOT NULL DEFAULT '',
    recipient   TEXT NOT NULL DEFAULT '',   -- age1...
    schedule    TEXT NOT NULL DEFAULT '',   -- cron; '' = on demand only
    retention   INTEGER NOT NULL DEFAULT 14 CHECK (retention BETWEEN 1 AND 1000),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT state_backup_settings_ready_chk
        CHECK (NOT enabled OR (bucket <> '' AND recipient <> ''))
);

-- One row per finished run, for the page and the digest. Kept 30 days.
CREATE TABLE state_backup_runs (
    id            BIGSERIAL PRIMARY KEY,
    trigger       TEXT NOT NULL CHECK (trigger IN ('schedule', 'manual', 'pre_update')),
    status        TEXT NOT NULL CHECK (status IN ('ok', 'error')),
    object_key    TEXT NOT NULL DEFAULT '',
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    from_version  TEXT NOT NULL DEFAULT '',
    to_version    TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX state_backup_runs_finished_idx ON state_backup_runs (finished_at);
