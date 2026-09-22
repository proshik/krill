-- App metrics (observability stage 2): whether Krill's apps collector scrapes
-- this app, the bearer token it sends (secret.Enc; injected into the app's env
-- at deploy under metrics_token_env), and the endpoints to scrape.
ALTER TABLE applications ADD COLUMN metrics_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE applications ADD COLUMN metrics_token text;
ALTER TABLE applications ADD COLUMN metrics_token_env text NOT NULL DEFAULT 'KRILL_METRICS_TOKEN';

CREATE TABLE app_metrics_endpoints (
    id             BIGSERIAL PRIMARY KEY,
    application_id BIGINT  NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    port           INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    path           TEXT    NOT NULL DEFAULT '/metrics' CHECK (path LIKE '/%'),
    job            TEXT    NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, port, path)
);
CREATE INDEX app_metrics_endpoints_application_id_idx ON app_metrics_endpoints (application_id);
