-- ClearOldDeploymentLogs runs every 10 minutes and scanned the whole
-- append-only deployments table to find the few rows still holding a log.
-- A partial index matches exactly the rows the UPDATE targets, so the sweep
-- touches nothing once the backlog is cleared.
CREATE INDEX IF NOT EXISTS deployments_log_cleanup_idx
    ON deployments (finished_at)
    WHERE finished_at IS NOT NULL AND log <> '';
