-- A fingerprint of the configuration a successful deployment applied, one
-- digest per settings section (source, env, runtime, volumes, ...), so the UI
-- can tell which saved settings are not live yet. Digests only, never the
-- values (HMAC-SHA256 under a subkey of KRILL_SECRET_KEY when it is set).
-- NULL for failed deployments and for those made before it was recorded,
-- which read as "unknown", not as changed.
ALTER TABLE deployments ADD COLUMN applied_config JSONB;
