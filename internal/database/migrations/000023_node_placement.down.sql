ALTER TABLE redis_dbs DROP COLUMN node_hostname;
ALTER TABLE postgres_dbs DROP COLUMN node_hostname;
ALTER TABLE applications DROP COLUMN placement_nodes;
ALTER TABLE applications DROP COLUMN placement_mode;
