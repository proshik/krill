-- env_text stores the environment variables as raw, ORDER-PRESERVING KEY=VALUE
-- lines (what the user typed). The existing `env` JSONB map stays the source for
-- deployment (env is a set there, order irrelevant); env_text is only for the
-- editor so it no longer re-sorts the user's input alphabetically.
ALTER TABLE applications ADD COLUMN env_text TEXT NOT NULL DEFAULT '';

-- Backfill from the existing map (sorted — legacy rows had no stored order).
UPDATE applications SET env_text = COALESCE(
    (SELECT string_agg(key || '=' || value, E'\n' ORDER BY key)
     FROM jsonb_each_text(env)), '');
