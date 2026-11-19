DROP TABLE IF EXISTS notify_alert_state;
DROP TABLE IF EXISTS backup_runs;
DROP TABLE IF EXISTS node_disks;
ALTER TABLE notification_channels
    DROP COLUMN IF EXISTS lang,
    DROP COLUMN IF EXISTS digest_last_at,
    DROP COLUMN IF EXISTS digest_tz,
    DROP COLUMN IF EXISTS digest_minute,
    DROP COLUMN IF EXISTS digest_enabled,
    DROP COLUMN IF EXISTS mem_threshold_pct,
    DROP COLUMN IF EXISTS disk_threshold_pct,
    DROP COLUMN IF EXISTS notify_infra,
    DROP COLUMN IF EXISTS notify_deploy_success;
