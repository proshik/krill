-- Systems: named groups of an environment's apps (Penpot, lets-project, ...)
-- for the environment view. A system owns nothing: deleting one removes only
-- the grouping, never an app, a database, a bucket or a server. An app belongs
-- to at most one system (application_id is the key). New tables only: a
-- release before this one ignores them.
CREATE TABLE systems (
    id BIGSERIAL PRIMARY KEY,
    environment_id BIGINT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (environment_id, name)
);

CREATE TABLE system_members (
    application_id BIGINT PRIMARY KEY REFERENCES applications(id) ON DELETE CASCADE,
    system_id BIGINT NOT NULL REFERENCES systems(id) ON DELETE CASCADE
);
CREATE INDEX system_members_system_id_idx ON system_members (system_id);
