-- name: GetObservabilitySettings :one
SELECT * FROM observability_settings WHERE id = 1;

-- name: SaveObservabilitySettings :exec
INSERT INTO observability_settings (id, metrics_url, metrics_user, metrics_password, logs_url, logs_user, logs_password, updated_at)
VALUES (1, $1, $2, $3, $4, $5, $6, now())
ON CONFLICT (id) DO UPDATE SET
    metrics_url      = EXCLUDED.metrics_url,
    metrics_user     = EXCLUDED.metrics_user,
    metrics_password = EXCLUDED.metrics_password,
    logs_url         = EXCLUDED.logs_url,
    logs_user        = EXCLUDED.logs_user,
    logs_password    = EXCLUDED.logs_password,
    updated_at       = now();

-- name: SetObservabilityEnabled :execrows
UPDATE observability_settings SET enabled = $1, updated_at = now() WHERE id = 1;
