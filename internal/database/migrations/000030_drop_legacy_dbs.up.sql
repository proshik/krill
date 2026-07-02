-- Finishes the instance-model conversion started in 000029: drops the legacy
-- one-container-one-database tables and the transitional link/backup columns.
ALTER TABLE app_db_links
    DROP COLUMN engine,
    DROP COLUMN db_id;
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_target_chk
    CHECK ((logical_database_id IS NULL) <> (instance_id IS NULL));

ALTER TABLE backups
    ALTER COLUMN logical_database_id SET NOT NULL,
    DROP COLUMN postgres_db_id;

ALTER TABLE db_instances DROP COLUMN legacy_pg_id, DROP COLUMN legacy_redis_id;
ALTER TABLE logical_databases DROP COLUMN legacy_pg_id;

DROP TABLE postgres_dbs;
DROP TABLE redis_dbs;
