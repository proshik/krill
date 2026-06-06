-- WARNING: destructive — drops the org-hierarchy tables and ALL their data.
-- Down migrations are for local/dev rollback only; Krill applies migrations up-only on startup.
DROP TABLE IF EXISTS applications;
CREATE TABLE applications (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    image      TEXT NOT NULL,
    tag        TEXT NOT NULL DEFAULT 'latest',
    domain     TEXT NOT NULL,
    port       INTEGER NOT NULL,
    env        JSONB NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'idle',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TABLE IF EXISTS environments;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS members;
DROP TABLE IF EXISTS organizations;
