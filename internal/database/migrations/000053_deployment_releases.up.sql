CREATE TABLE deployment_releases (
    deployment_id BIGINT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    application_id BIGINT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    payload TEXT NOT NULL,
    confirmed BOOLEAN NOT NULL DEFAULT false,
    source_deployment_id BIGINT REFERENCES deployments(id) ON DELETE CASCADE
);

ALTER TABLE deployments ADD COLUMN rollback_source_id BIGINT;
