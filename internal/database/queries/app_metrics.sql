-- name: GetApplicationMetrics :one
SELECT metrics_enabled, metrics_token, metrics_token_env, port FROM applications WHERE id = $1;

-- name: SetApplicationMetricsEnabled :exec
UPDATE applications SET metrics_enabled = $2, updated_at = now() WHERE id = $1;

-- name: SetApplicationMetricsToken :exec
UPDATE applications SET metrics_token = $2, updated_at = now() WHERE id = $1;

-- name: SetApplicationMetricsTokenEnv :exec
UPDATE applications SET metrics_token_env = $2, updated_at = now() WHERE id = $1;

-- name: ListMetricsEndpointsByApplication :many
SELECT * FROM app_metrics_endpoints WHERE application_id = $1 ORDER BY id;

-- name: GetMetricsEndpoint :one
SELECT * FROM app_metrics_endpoints WHERE id = $1;

-- name: CreateMetricsEndpoint :one
INSERT INTO app_metrics_endpoints (application_id, port, path, job)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: DeleteMetricsEndpoint :exec
DELETE FROM app_metrics_endpoints WHERE id = $1 AND application_id = $2;

-- name: CountMetricsEndpointsByApplication :one
SELECT count(*) FROM app_metrics_endpoints WHERE application_id = $1;

-- name: CountMetricsEndpointsByPort :one
SELECT count(*) FROM app_metrics_endpoints WHERE application_id = $1 AND port = $2;

-- name: ListMetricsScrapeTargets :many
-- Every endpoint of every app with metrics on and a token, with the names and
-- ids its series are labelled with. Ordered so the rendered module is stable.
SELECT a.id AS app_id, a.name AS app_name, a.metrics_token,
       e.name AS env_name, p.name AS project_name,
       o.id AS org_id, o.name AS org_name,
       ep.id AS endpoint_id, ep.port, ep.path, ep.job
FROM app_metrics_endpoints ep
JOIN applications a ON a.id = ep.application_id
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
JOIN organizations o ON o.id = p.organization_id
WHERE a.metrics_enabled AND a.metrics_token IS NOT NULL
ORDER BY a.id, ep.id;
