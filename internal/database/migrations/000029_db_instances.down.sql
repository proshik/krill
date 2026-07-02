-- Reverts the instance model. Data created only in the new tables is LOST.
ALTER TABLE backups DROP COLUMN logical_database_id;
ALTER TABLE backups ALTER COLUMN postgres_db_id DROP DEFAULT;
ALTER TABLE backups ADD CONSTRAINT backups_postgres_db_id_fkey
    FOREIGN KEY (postgres_db_id) REFERENCES postgres_dbs(id) ON DELETE CASCADE;

ALTER TABLE app_db_links
    DROP COLUMN logical_database_id,
    DROP COLUMN instance_id,
    ALTER COLUMN engine DROP DEFAULT,
    ALTER COLUMN db_id  DROP DEFAULT;
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_engine_check
    CHECK (engine IN ('postgres','redis'));

DROP TABLE logical_databases;
DROP TABLE db_instances;
