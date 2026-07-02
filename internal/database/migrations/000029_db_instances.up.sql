-- DBMS instances (org-level database servers) + logical databases inside them.
-- Converts the legacy one-container-one-database model (postgres_dbs/redis_dbs).
-- The legacy tables are kept intact here and dropped by migration 000030 (same
-- release); legacy_* mapping columns are dropped there too.

CREATE TABLE db_instances (
    id                 BIGSERIAL PRIMARY KEY,
    organization_id    BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    engine             TEXT NOT NULL CHECK (engine IN ('postgres','redis')),
    name               TEXT NOT NULL,
    app_name           TEXT NOT NULL UNIQUE,
    image              TEXT NOT NULL,
    superuser          TEXT NOT NULL DEFAULT '',
    superuser_password TEXT NOT NULL,
    external_port      INTEGER,
    node_hostname      TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'idle' CHECK (status IN ('idle','running','error')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    legacy_pg_id       BIGINT,
    legacy_redis_id    BIGINT,
    UNIQUE (organization_id, name)
);
CREATE UNIQUE INDEX idx_db_instances_external_port ON db_instances(external_port)
    WHERE external_port IS NOT NULL;
CREATE INDEX db_instances_org_id_idx ON db_instances(organization_id);

CREATE TABLE logical_databases (
    id             BIGSERIAL PRIMARY KEY,
    instance_id    BIGINT NOT NULL REFERENCES db_instances(id) ON DELETE RESTRICT,
    environment_id BIGINT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    db_name        TEXT NOT NULL,
    username       TEXT NOT NULL,
    password       TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    legacy_pg_id   BIGINT,
    UNIQUE (instance_id, db_name),
    UNIQUE (environment_id, name)
);
CREATE INDEX logical_databases_env_id_idx ON logical_databases(environment_id);
CREATE INDEX logical_databases_instance_id_idx ON logical_databases(instance_id);

-- Convert every legacy container (postgres + redis) into an instance in a
-- single INSERT over a UNION ALL of both legacy tables. POSTGRES_USER in the
-- official image is the server superuser, so its creds become the instance
-- creds (ciphertext moved as-is); redis has no superuser concept, so
-- `superuser` stays ''. The union must happen BEFORE the collision window
-- function runs so it sees every name in the org across BOTH engines:
-- db_instances has UNIQUE(organization_id, name) spanning postgres AND
-- redis, so computing the window per engine (two separate INSERTs, as an
-- earlier version of this migration did) misses a same-org, same-name
-- collision between a postgres db and a redis db and dies on the UNIQUE
-- violation. The suffix itself must be engine-qualified, not just `-<id>`:
-- a bare `-<id>` can still collide across engines (postgres id=5 and redis
-- id=5, both named 'cache', would otherwise both become 'cache-5').
INSERT INTO db_instances (organization_id, engine, name, app_name, image, superuser,
                          superuser_password, external_port, node_hostname, status,
                          created_at, updated_at, legacy_pg_id, legacy_redis_id)
SELECT organization_id, engine,
       CASE WHEN count(*) OVER (PARTITION BY organization_id, name) > 1
            THEN name || '-' || engine || '-' || COALESCE(legacy_pg_id, legacy_redis_id)::text
            ELSE name END,
       app_name, image, superuser, superuser_password, external_port, node_hostname,
       status, created_at, updated_at, legacy_pg_id, legacy_redis_id
FROM (
    SELECT p.organization_id, 'postgres' AS engine, pd.name, pd.app_name, pd.image,
           pd.database_user AS superuser, pd.database_password AS superuser_password,
           pd.external_port, pd.node_hostname, pd.status, pd.created_at, pd.updated_at,
           pd.id AS legacy_pg_id, NULL::bigint AS legacy_redis_id
    FROM postgres_dbs pd
    JOIN environments e ON e.id = pd.environment_id
    JOIN projects p     ON p.id = e.project_id
    UNION ALL
    SELECT p.organization_id, 'redis' AS engine, rd.name, rd.app_name, rd.image,
           '' AS superuser, rd.password AS superuser_password,
           rd.external_port, rd.node_hostname, rd.status, rd.created_at, rd.updated_at,
           NULL::bigint AS legacy_pg_id, rd.id AS legacy_redis_id
    FROM redis_dbs rd
    JOIN environments e ON e.id = rd.environment_id
    JOIN projects p     ON p.id = e.project_id
) legacy;

-- One logical database per converted postgres container (same creds as the
-- superuser — matches today's behavior, no regression).
INSERT INTO logical_databases (instance_id, environment_id, name, db_name,
                               username, password, created_at, legacy_pg_id)
SELECT di.id, pd.environment_id, pd.name, pd.database_name,
       pd.database_user, pd.database_password, pd.created_at, pd.id
FROM postgres_dbs pd
JOIN db_instances di ON di.legacy_pg_id = pd.id;

-- Rewire app_db_links onto real FKs. The legacy engine/db_id columns stay until
-- 000030; DEFAULTs let new-style inserts omit them, and the engine CHECK must go
-- because the default '' is outside its allowed set.
ALTER TABLE app_db_links
    ADD COLUMN logical_database_id BIGINT REFERENCES logical_databases(id) ON DELETE CASCADE,
    ADD COLUMN instance_id         BIGINT REFERENCES db_instances(id)      ON DELETE CASCADE,
    ALTER COLUMN engine SET DEFAULT '',
    ALTER COLUMN db_id  SET DEFAULT 0;
ALTER TABLE app_db_links DROP CONSTRAINT IF EXISTS app_db_links_engine_check;

UPDATE app_db_links l SET logical_database_id = ld.id
FROM logical_databases ld WHERE l.engine = 'postgres' AND ld.legacy_pg_id = l.db_id;

UPDATE app_db_links l SET instance_id = di.id
FROM db_instances di WHERE l.engine = 'redis' AND di.legacy_redis_id = l.db_id;

-- Links whose target DB row vanished were already dead (resolveDBLinkURL skipped
-- them at deploy) — drop them instead of carrying unmapped rows forward.
DELETE FROM app_db_links WHERE logical_database_id IS NULL AND instance_id IS NULL;

CREATE INDEX app_db_links_ldb_idx  ON app_db_links(logical_database_id);
CREATE INDEX app_db_links_inst_idx ON app_db_links(instance_id);

-- Rewire backups. postgres_db_id keeps NOT NULL via DEFAULT 0 until 000030; its
-- FK must go so new-style inserts (which omit it) don't violate it.
ALTER TABLE backups
    ADD COLUMN logical_database_id BIGINT REFERENCES logical_databases(id) ON DELETE CASCADE,
    ALTER COLUMN postgres_db_id SET DEFAULT 0;
ALTER TABLE backups DROP CONSTRAINT backups_postgres_db_id_fkey;

UPDATE backups b SET logical_database_id = ld.id
FROM logical_databases ld WHERE ld.legacy_pg_id = b.postgres_db_id;

CREATE INDEX backups_logical_database_id_idx ON backups(logical_database_id);
