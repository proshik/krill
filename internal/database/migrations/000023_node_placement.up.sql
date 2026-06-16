ALTER TABLE applications ADD COLUMN placement_mode  TEXT NOT NULL DEFAULT 'any';  -- any | pin | global
ALTER TABLE applications ADD COLUMN placement_nodes TEXT NOT NULL DEFAULT '';      -- CSV of swarm node IDs
ALTER TABLE postgres_dbs ADD COLUMN node_hostname TEXT NOT NULL DEFAULT '';        -- '' = control-plane (manager)
ALTER TABLE redis_dbs    ADD COLUMN node_hostname TEXT NOT NULL DEFAULT '';        -- '' = control-plane (manager)
