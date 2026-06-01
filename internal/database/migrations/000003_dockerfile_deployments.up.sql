ALTER TABLE applications
  ADD COLUMN source_type     TEXT NOT NULL DEFAULT 'image'
    CHECK (source_type IN ('image','dockerfile')),
  ADD COLUMN git_url         TEXT NOT NULL DEFAULT '',
  ADD COLUMN git_branch      TEXT NOT NULL DEFAULT '',
  ADD COLUMN dockerfile_path TEXT NOT NULL DEFAULT 'Dockerfile';

CREATE TABLE deployments (
    id             BIGSERIAL PRIMARY KEY,
    application_id BIGINT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    status         TEXT NOT NULL DEFAULT 'running'
       CHECK (status IN ('running','done','error')),
    trigger        TEXT NOT NULL DEFAULT 'manual'
       CHECK (trigger IN ('manual','webhook','schedule')),
    image_tag      TEXT NOT NULL DEFAULT '',
    log            TEXT NOT NULL DEFAULT '',
    error_message  TEXT NOT NULL DEFAULT '',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ
);
CREATE INDEX deployments_app_idx ON deployments(application_id, started_at DESC);
