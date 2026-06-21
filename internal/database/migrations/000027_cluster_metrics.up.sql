-- Cluster-wide monitoring: tag each metric sample with the node it came from,
-- and keep a per-node capacity row (CPU count + total memory) for the used/total
-- denominators without SSH-ing to every node on each page load.
ALTER TABLE metric_samples ADD COLUMN node TEXT NOT NULL DEFAULT '';
CREATE INDEX metric_samples_node_component_ts_idx ON metric_samples (node, component, ts);

CREATE TABLE node_capacity (
    node            TEXT PRIMARY KEY,
    ncpu            INTEGER     NOT NULL DEFAULT 0,
    mem_total_bytes BIGINT      NOT NULL DEFAULT 0,
    sampled_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
