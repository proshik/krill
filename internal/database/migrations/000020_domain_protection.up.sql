ALTER TABLE domains ADD COLUMN basic_auth_users TEXT NOT NULL DEFAULT '';
ALTER TABLE domains ADD COLUMN allowed_ips      TEXT NOT NULL DEFAULT '';
