-- A second host-published port per instance (e.g. MinIO's console :9001,
-- alongside the S3 API on external_port :9000). Nullable — only engines whose
-- driver exposes a second ProxyTarget ("-console" suffix) ever set it.
ALTER TABLE db_instances ADD COLUMN console_external_port INTEGER;
CREATE UNIQUE INDEX idx_db_instances_console_ext_port ON db_instances(console_external_port)
    WHERE console_external_port IS NOT NULL;
