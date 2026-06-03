CREATE TABLE registries (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    registry_url    TEXT NOT NULL,
    username        TEXT NOT NULL,
    password        TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX registries_org_name_key ON registries(organization_id, name);

ALTER TABLE applications ADD COLUMN registry_id BIGINT REFERENCES registries(id) ON DELETE SET NULL;
