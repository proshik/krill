CREATE UNIQUE INDEX IF NOT EXISTS idx_postgres_dbs_external_port
    ON postgres_dbs (external_port) WHERE external_port IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_redis_dbs_external_port
    ON redis_dbs (external_port) WHERE external_port IS NOT NULL;
