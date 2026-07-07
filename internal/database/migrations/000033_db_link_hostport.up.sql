-- Extend the db-link field allow-list with 'hostport' (host:port in one value).
ALTER TABLE app_db_links DROP CONSTRAINT app_db_links_field_chk;
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_field_chk
    CHECK (field IN ('url','password','host','port','user','dbname','hostport'));
