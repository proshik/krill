-- A third token level, 'provision': everything 'write' can do, plus creating
-- logical databases, image apps, domains and database links through the agent
-- API. A separate level keeps existing CI 'write' tokens from gaining creation
-- rights. The inline CHECK from 000040 is Postgres-auto-named
-- api_tokens_level_check; recreate it wider. The previous release compares the
-- level with 'write' only, so after a rollback a 'provision' token is read-only
-- there — it never gains rights it should not have.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_level_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_level_check
    CHECK (level IN ('read', 'write', 'provision'));
