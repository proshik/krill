-- Success notifications, resource thresholds and the daily digest.
-- Every change is additive: new columns carry a DEFAULT and new tables are
-- new. The previous release never writes the new columns (its upsert leaves
-- them untouched), so their CHECKs cannot reject anything it writes.
ALTER TABLE notification_channels
    ADD COLUMN notify_deploy_success BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN notify_infra BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN disk_threshold_pct SMALLINT NOT NULL DEFAULT 85
        CONSTRAINT notification_channels_disk_threshold_chk CHECK (disk_threshold_pct BETWEEN 50 AND 99),
    ADD COLUMN mem_threshold_pct SMALLINT NOT NULL DEFAULT 100
        CONSTRAINT notification_channels_mem_threshold_chk CHECK (mem_threshold_pct BETWEEN 50 AND 200),
    ADD COLUMN digest_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN digest_minute SMALLINT NOT NULL DEFAULT 660
        CONSTRAINT notification_channels_digest_minute_chk CHECK (digest_minute BETWEEN 0 AND 1439),
    ADD COLUMN digest_tz TEXT NOT NULL DEFAULT 'Europe/Moscow',
    ADD COLUMN digest_last_at TIMESTAMPTZ,
    ADD COLUMN lang TEXT NOT NULL DEFAULT 'en'
        CONSTRAINT notification_channels_lang_chk CHECK (lang IN ('en', 'ru'));

-- The last disk snapshot of every node's root and Docker filesystems.
CREATE TABLE node_disks (
    node         TEXT NOT NULL,
    mount        TEXT NOT NULL,
    filesystem   TEXT NOT NULL DEFAULT '',
    total_bytes  BIGINT NOT NULL,
    used_bytes   BIGINT NOT NULL,
    avail_bytes  BIGINT NOT NULL,
    sampled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (node, mount)
);

-- One row per finished backup run of any kind, for the daily digest. The
-- owner's location is resolved and copied when the run finishes, so a row
-- survives its backup config being deleted. Kept for 30 days.
CREATE TABLE backup_runs (
    id          BIGSERIAL PRIMARY KEY,
    org_id      BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('database', 'volume', 'bucket')),
    source_id   BIGINT NOT NULL,
    project     TEXT NOT NULL DEFAULT '',
    env         TEXT NOT NULL DEFAULT '',
    target      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL CHECK (status IN ('ok', 'error')),
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    finished_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX backup_runs_org_finished_idx ON backup_runs (org_id, finished_at);
CREATE INDEX backup_runs_finished_idx ON backup_runs (finished_at);

-- Per-channel state of a threshold alert (e.g. "disk:worker-1:/"), so a
-- restart neither repeats an alert nor forgets to send its recovery.
CREATE TABLE notify_alert_state (
    channel_id   BIGINT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    alert_key    TEXT NOT NULL,
    active       BOOLEAN NOT NULL DEFAULT false,
    last_sent_at TIMESTAMPTZ,
    PRIMARY KEY (channel_id, alert_key)
);
