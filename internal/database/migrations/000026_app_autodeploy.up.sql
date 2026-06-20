-- Per-app auto-deploy: an opt-in toggle plus a secret that doubles as the GitHub
-- webhook HMAC secret (Dockerfile/git apps) and the deploy-hook bearer token
-- (image apps). NOT NULL DEFAULT '' keeps the sqlc type a plain string.
ALTER TABLE applications
    ADD COLUMN auto_deploy    BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN webhook_secret TEXT    NOT NULL DEFAULT '';
