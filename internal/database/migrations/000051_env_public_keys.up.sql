-- Only explicitly classified ordinary keys are visible. Legacy and new keys
-- remain secrets by default; env_text remains the deployment source of truth.
ALTER TABLE applications ADD COLUMN env_public_keys TEXT[] NOT NULL DEFAULT '{}';
