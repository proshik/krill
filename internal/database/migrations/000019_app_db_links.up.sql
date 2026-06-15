CREATE TABLE app_db_links (
    id             BIGSERIAL PRIMARY KEY,
    application_id BIGINT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    engine         TEXT   NOT NULL CHECK (engine IN ('postgres','redis')),
    db_id          BIGINT NOT NULL,
    var_name       TEXT   NOT NULL,
    scheme         TEXT   NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX app_db_links_app_var_key ON app_db_links(application_id, var_name);
CREATE INDEX app_db_links_app_id_idx ON app_db_links(application_id);
CREATE INDEX app_db_links_db_idx ON app_db_links(engine, db_id);
