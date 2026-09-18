-- Krill's own observability agent (internal/observability). One row for the
-- whole instance: the agent runs on every node, whatever organization owns
-- the workloads there.
--
-- The *_password columns hold secret.Enc output. An empty *_url turns that
-- pipeline off; an empty *_user turns basic auth off.
CREATE TABLE observability_settings (
    id               SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    enabled          BOOLEAN NOT NULL DEFAULT false,
    metrics_url      TEXT NOT NULL DEFAULT '',
    metrics_user     TEXT NOT NULL DEFAULT '',
    metrics_password TEXT NOT NULL DEFAULT '',
    logs_url         TEXT NOT NULL DEFAULT '',
    logs_user        TEXT NOT NULL DEFAULT '',
    logs_password    TEXT NOT NULL DEFAULT '',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- An enabled agent with nowhere to send anything is a misconfiguration.
    CONSTRAINT observability_settings_target_chk CHECK (NOT enabled OR metrics_url <> '' OR logs_url <> '')
);
