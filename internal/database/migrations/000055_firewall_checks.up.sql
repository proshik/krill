-- Last successful live check of each node's firewall, shown when the node
-- cannot be reached. node is "control-plane" or "node-<cluster_nodes.id>".
CREATE TABLE firewall_checks (
    node TEXT PRIMARY KEY,
    checked_at TIMESTAMPTZ NOT NULL
);
