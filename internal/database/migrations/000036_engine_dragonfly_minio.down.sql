-- Revert to postgres/redis only. Any dragonfly/minio row would violate the
-- restored constraint (matches the fail-closed convention of prior migrations
-- in this table's history — down migrations are for dev rollback, not applied
-- against live dragonfly/minio data).
ALTER TABLE db_instances DROP CONSTRAINT db_instances_engine_check;
ALTER TABLE db_instances ADD CONSTRAINT db_instances_engine_check
    CHECK (engine IN ('postgres', 'redis'));
