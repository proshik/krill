-- Revert to the pre-hostport allow-list. Any existing 'hostport' rows would
-- violate this constraint on rollback (dev-only down migration).
ALTER TABLE app_db_links DROP CONSTRAINT app_db_links_field_chk;
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_field_chk
    CHECK (field IN ('url','password','host','port','user','dbname'));
