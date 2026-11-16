-- A non-primary domain can answer every request with a permanent redirect to
-- its application's current primary domain. The previous release never writes
-- this column, so the CHECK cannot reject anything it writes.
ALTER TABLE domains
    ADD COLUMN redirect_to_primary BOOLEAN NOT NULL DEFAULT false
    CONSTRAINT domains_redirect_not_primary CHECK (NOT (is_primary AND redirect_to_primary));
