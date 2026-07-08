-- Extend the db-link field allow-list with the S3 fields a minio app-link can
-- target (endpoint/access_key/secret_key/region), alongside the existing
-- postgres/redis-shaped fields.
ALTER TABLE app_db_links DROP CONSTRAINT app_db_links_field_chk;
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_field_chk
    CHECK (field IN ('url','password','host','port','user','dbname','hostport','endpoint','access_key','secret_key','region'));
