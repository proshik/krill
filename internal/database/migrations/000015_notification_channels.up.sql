CREATE TABLE notification_channels (
    id             BIGSERIAL PRIMARY KEY,
    org_id         BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    type           TEXT NOT NULL DEFAULT 'telegram' CHECK (type IN ('telegram')),
    enabled        BOOLEAN NOT NULL DEFAULT true,
    bot_token      TEXT NOT NULL DEFAULT '',
    chat_id        TEXT NOT NULL DEFAULT '',
    notify_deploy  BOOLEAN NOT NULL DEFAULT true,
    notify_backup  BOOLEAN NOT NULL DEFAULT true,
    notify_health  BOOLEAN NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX notification_channels_org_type_uq ON notification_channels(org_id, type);
