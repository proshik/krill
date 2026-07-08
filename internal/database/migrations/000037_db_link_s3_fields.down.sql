-- Revert to the pre-S3 allow-list. Any existing S3-field ('endpoint',
-- 'access_key', 'secret_key', 'region') rows would violate this constraint on
-- rollback (dev-only down migration).
ALTER TABLE app_db_links DROP CONSTRAINT app_db_links_field_chk;
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_field_chk
    CHECK (field IN ('url','password','host','port','user','dbname','hostport'));
