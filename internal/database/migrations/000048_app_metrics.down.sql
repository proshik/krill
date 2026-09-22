DROP TABLE IF EXISTS app_metrics_endpoints;
ALTER TABLE applications DROP COLUMN IF EXISTS metrics_token_env;
ALTER TABLE applications DROP COLUMN IF EXISTS metrics_token;
ALTER TABLE applications DROP COLUMN IF EXISTS metrics_enabled;
