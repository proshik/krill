-- Krill link, stage 1 egress (KRL-002): the relay's destination list, the
-- client's organization grants and each app's selection. All tables are new,
-- so their primary keys and foreign keys are additive; a release without
-- egress ignores them.

-- relay: the one destination list, the hard boundary for every peer.
CREATE TABLE link_destinations (
  host       TEXT NOT NULL,
  port       INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (host, port)
);

-- client: organizations the operator opened the link to.
CREATE TABLE link_org_grants (
  org_id     BIGINT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
  granted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- client: the destinations an app sends through the link (a subset of what the
-- relay sent). No foreign key to the relay's list: it lives on another
-- installation.
CREATE TABLE app_link_destinations (
  application_id BIGINT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
  host           TEXT NOT NULL,
  port           INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
  PRIMARY KEY (application_id, host, port)
);
