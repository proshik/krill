-- WARNING: destructive. Dropping this table revokes every issued API token;
-- clients must be re-issued new ones after a re-migrate.
DROP TABLE IF EXISTS api_tokens;
