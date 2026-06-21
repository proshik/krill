DROP TABLE node_capacity;
DROP INDEX metric_samples_node_component_ts_idx;
ALTER TABLE metric_samples DROP COLUMN node;
