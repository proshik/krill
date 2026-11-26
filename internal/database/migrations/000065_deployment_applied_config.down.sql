DROP INDEX IF EXISTS deployments_done_idx;
DROP INDEX IF EXISTS deployments_running_idx;
ALTER TABLE deployments DROP COLUMN IF EXISTS applied_config;
