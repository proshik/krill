-- Records that every service of the organization has been moved onto its own
-- overlay network: its database instances came back up and every app
-- deployment finished without an error (a submitted deploy proves nothing; one
-- that fails leaves the app on the old network). network_name alone cannot say
-- this: it is written BEFORE the services move (the deployer reads it to know
-- where to deploy), so a pass that failed halfway would otherwise look complete
-- and never run again, leaving the rest of the tenant on the shared network.
ALTER TABLE organizations ADD COLUMN network_migrated_at TIMESTAMPTZ;
