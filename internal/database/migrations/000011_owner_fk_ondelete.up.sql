ALTER TABLE organizations DROP CONSTRAINT organizations_owner_id_fkey;
ALTER TABLE organizations ADD CONSTRAINT organizations_owner_id_fkey
    FOREIGN KEY (owner_id) REFERENCES users(id) ON DELETE RESTRICT;
