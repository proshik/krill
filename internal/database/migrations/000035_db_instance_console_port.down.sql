DROP INDEX IF EXISTS idx_db_instances_console_ext_port;
ALTER TABLE db_instances DROP COLUMN console_external_port;
