-- Extend the engine allow-list to include DragonFly (Redis-compatible,
-- Task 3) and MinIO (S3-compatible, Task 4) alongside postgres/redis. The
-- inline column CHECK from 000029 is Postgres-auto-named
-- db_instances_engine_check; recreate it rather than adding a second CHECK.
ALTER TABLE db_instances DROP CONSTRAINT db_instances_engine_check;
ALTER TABLE db_instances ADD CONSTRAINT db_instances_engine_check
    CHECK (engine IN ('postgres', 'redis', 'dragonfly', 'minio'));
