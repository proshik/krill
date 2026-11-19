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

-- name: UpsertNodeDisk :exec
INSERT INTO node_disks (node, mount, filesystem, total_bytes, used_bytes, avail_bytes, sampled_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (node, mount) DO UPDATE
  SET filesystem = EXCLUDED.filesystem, total_bytes = EXCLUDED.total_bytes,
      used_bytes = EXCLUDED.used_bytes, avail_bytes = EXCLUDED.avail_bytes, sampled_at = now();

-- name: ListNodeDisks :many
SELECT node, mount, filesystem, total_bytes, used_bytes, avail_bytes, sampled_at
FROM node_disks ORDER BY node, mount;

-- name: PruneNodeDisksExcept :exec
-- Drops disks of nodes no longer in the cluster.
DELETE FROM node_disks WHERE node <> ALL($1::text[]);

-- name: PruneNodeDiskMounts :exec
-- Drops a node's mounts that its latest snapshot no longer reports.
DELETE FROM node_disks WHERE node = $1 AND mount <> ALL(sqlc.arg(mounts)::text[]);

-- name: PruneNodeDisks :exec
DELETE FROM node_disks WHERE sampled_at < $1;
