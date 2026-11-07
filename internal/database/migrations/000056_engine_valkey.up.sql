-- Allow the valkey engine. Widening the allow-list rejects nothing an earlier
-- release writes. A release before this one has no valkey driver: it lists and
-- shows a valkey instance, but cannot deploy, start or delete it.
ALTER TABLE db_instances DROP CONSTRAINT db_instances_engine_check;
ALTER TABLE db_instances ADD CONSTRAINT db_instances_engine_check
    CHECK (engine IN ('postgres', 'redis', 'dragonfly', 'minio', 'valkey'));
