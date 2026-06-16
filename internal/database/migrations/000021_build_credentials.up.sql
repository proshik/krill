CREATE TABLE git_credentials (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    host            TEXT NOT NULL,
    username        TEXT NOT NULL,
    token           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX git_credentials_org_name_key ON git_credentials(organization_id, name);
CREATE INDEX idx_git_credentials_org ON git_credentials(organization_id);

ALTER TABLE applications ADD COLUMN git_credential_id BIGINT REFERENCES git_credentials(id) ON DELETE SET NULL;
ALTER TABLE applications ADD COLUMN build_args    TEXT NOT NULL DEFAULT '';
ALTER TABLE applications ADD COLUMN build_secrets TEXT NOT NULL DEFAULT '';
