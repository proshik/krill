ALTER TABLE app_db_links ADD COLUMN field TEXT NOT NULL DEFAULT 'url';
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_field_chk
    CHECK (field IN ('url','password','host','port','user','dbname'));
