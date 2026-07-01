-- Instance-level operator flag. Global infrastructure (cluster nodes, the
-- host-wide monitoring view) is gated on this flag rather than on org-scoped
-- RoleAdmin, because org-admin is self-grantable (any user can create an org
-- via POST /orgs and becomes its owner). The seeded admin is promoted to
-- is_admin=true on startup (SeedAdmin); everyone else defaults to false.
ALTER TABLE users
    ADD COLUMN is_admin BOOLEAN NOT NULL DEFAULT false;
