-- Provision tokens become read tokens before the narrower constraint is
-- restored: the older binary must not gain anything from a downgrade, so the
-- fallback is the same as a plain binary rollback (read-only), not write.
UPDATE api_tokens SET level = 'read' WHERE level = 'provision';
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_level_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_level_check
    CHECK (level IN ('read', 'write'));
