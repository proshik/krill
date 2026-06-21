-- name: InsertMetricSample :exec
INSERT INTO metric_samples (node, component, cpu_pct, mem_bytes, mem_limit_bytes)
VALUES ($1, $2, $3, $4, $5);

-- name: MetricSamplesSince :many
SELECT node, component, ts, cpu_pct, mem_bytes, mem_limit_bytes
FROM metric_samples
WHERE ts >= $1
ORDER BY node, component, ts;

-- name: LatestMetricSamples :many
SELECT DISTINCT ON (node, component) node, component, ts, cpu_pct, mem_bytes, mem_limit_bytes
FROM metric_samples
WHERE ts >= $1
ORDER BY node, component, ts DESC;

-- name: PruneMetricSamples :exec
DELETE FROM metric_samples WHERE ts < $1;

-- name: UpsertNodeCapacity :exec
INSERT INTO node_capacity (node, ncpu, mem_total_bytes, sampled_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (node) DO UPDATE
  SET ncpu = EXCLUDED.ncpu, mem_total_bytes = EXCLUDED.mem_total_bytes, sampled_at = now();

-- name: ListNodeCapacity :many
SELECT node, ncpu, mem_total_bytes, sampled_at FROM node_capacity;

-- name: PruneNodeCapacity :exec
DELETE FROM node_capacity WHERE sampled_at < $1;

-- name: PruneNodeCapacityExcept :exec
DELETE FROM node_capacity WHERE node <> ALL($1::text[]);
