CREATE TABLE api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id       BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    level        TEXT NOT NULL CHECK (level IN ('read', 'write')),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Lookup is by prefix, then a constant-time compare of the full hash. The index
-- is deliberately NOT unique: an 8-char collision is unlikely but possible, and
-- failing token issuance over one would be worse than comparing two candidates.
CREATE INDEX idx_api_tokens_prefix ON api_tokens (prefix);
CREATE INDEX idx_api_tokens_user_id ON api_tokens (user_id);
CREATE INDEX idx_api_tokens_org_id ON api_tokens (org_id);
