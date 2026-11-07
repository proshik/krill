-- Fails while a valkey instance exists, rather than deleting it.
ALTER TABLE db_instances DROP CONSTRAINT db_instances_engine_check;
ALTER TABLE db_instances ADD CONSTRAINT db_instances_engine_check
    CHECK (engine IN ('postgres', 'redis', 'dragonfly', 'minio'));
