-- name: InsertMetricSample :exec
INSERT INTO metric_samples (component, cpu_pct, mem_bytes, mem_limit_bytes)
VALUES ($1, $2, $3, $4);

-- name: MetricSamplesSince :many
SELECT component, ts, cpu_pct, mem_bytes, mem_limit_bytes
FROM metric_samples WHERE ts >= $1 ORDER BY component, ts;

-- name: LatestMetricSamples :many
SELECT DISTINCT ON (component) component, ts, cpu_pct, mem_bytes, mem_limit_bytes
FROM metric_samples WHERE ts >= $1 ORDER BY component, ts DESC;

-- name: PruneMetricSamples :exec
DELETE FROM metric_samples WHERE ts < $1;
