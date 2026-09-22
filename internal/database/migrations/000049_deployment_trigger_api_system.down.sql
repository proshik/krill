-- Fold the new triggers back into 'manual', what they were recorded as before,
-- so the narrower constraint can be restored over existing history.
UPDATE deployments SET trigger = 'manual' WHERE trigger IN ('api', 'system');
ALTER TABLE deployments DROP CONSTRAINT deployments_trigger_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_trigger_check
    CHECK (trigger IN ('manual', 'webhook', 'schedule'));
