-- WARNING: drops per-domain basic-auth users and IP-allowlists.
ALTER TABLE domains DROP COLUMN allowed_ips;
ALTER TABLE domains DROP COLUMN basic_auth_users;
