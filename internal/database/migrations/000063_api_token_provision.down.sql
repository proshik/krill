-- Provision tokens become write tokens, the closest narrower level, so the
-- narrower constraint can be restored.
UPDATE api_tokens SET level = 'write' WHERE level = 'provision';
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_level_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_level_check
    CHECK (level IN ('read', 'write'));
