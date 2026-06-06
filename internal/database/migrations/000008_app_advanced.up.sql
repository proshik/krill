ALTER TABLE applications
    ADD COLUMN memory_limit             TEXT,
    ADD COLUMN cpu_limit                TEXT,
    ADD COLUMN replicas                 INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN restart_condition        TEXT    NOT NULL DEFAULT 'any'
        CHECK (restart_condition IN ('any','on-failure','none')),
    ADD COLUMN restart_max_attempts     INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN healthcheck_cmd          TEXT,
    ADD COLUMN healthcheck_interval     TEXT,
    ADD COLUMN healthcheck_timeout      TEXT,
    ADD COLUMN healthcheck_retries      INTEGER,
    ADD COLUMN healthcheck_start_period TEXT;
