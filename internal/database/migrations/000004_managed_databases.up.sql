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
    status            TEXT NOT NULL DEFAULT 'idle'
        CHECK (status IN ('idle','running','error')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
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
    status         TEXT NOT NULL DEFAULT 'idle'
        CHECK (status IN ('idle','running','error')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(environment_id, name)
);
