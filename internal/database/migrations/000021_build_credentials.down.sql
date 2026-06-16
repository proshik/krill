-- WARNING: drops git credentials and per-app build args/secrets.
ALTER TABLE applications DROP COLUMN build_secrets;
ALTER TABLE applications DROP COLUMN build_args;
ALTER TABLE applications DROP COLUMN git_credential_id;
DROP TABLE git_credentials;
