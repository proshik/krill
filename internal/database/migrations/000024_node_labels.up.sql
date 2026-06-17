-- Human-readable display labels for Swarm nodes, keyed by the Swarm node ID so
-- they cover every node uniformly (including the bootstrap leader, which has no
-- cluster_nodes row). Empty/absent = fall back to the raw hostname.
CREATE TABLE node_labels (
    swarm_node_id TEXT PRIMARY KEY,
    label         TEXT NOT NULL
);
