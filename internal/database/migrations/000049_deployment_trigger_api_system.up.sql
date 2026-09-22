-- Two more deployment triggers: 'api' for deploys requested with an agent-API
-- token (REST or MCP), 'system' for the redeploys the control plane submits for
-- itself (the organization network migration). Both used to be recorded as
-- 'manual'. The inline CHECK from 000003 is Postgres-auto-named
-- deployments_trigger_check; recreate it wider rather than adding a second one.
-- The previous release renders the trigger as plain text and never branches on
-- it, so it reads the new values fine after a rollback.
ALTER TABLE deployments DROP CONSTRAINT deployments_trigger_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_trigger_check
    CHECK (trigger IN ('manual', 'webhook', 'schedule', 'api', 'system'));
