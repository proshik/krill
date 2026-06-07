-- env_text (000013) is now the single source for environment variables; the
-- JSONB map duplicated the data. Drop it — the deploy map is derived from
-- env_text at runtime (dbservice/deploy parse it into a map[string]string).
ALTER TABLE applications DROP COLUMN env;
