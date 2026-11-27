-- Krill link, stage 1 core (KRL-001): one installation's role and key material,
-- the relay's peers and pairing codes, and the relay's ingress hosts. Every
-- private key is stored through secret.Enc; a pairing secret only as its
-- SHA-256. All tables are new, so their UNIQUE constraints and foreign keys are
-- additive.

-- One row: the installation's role and its key material.
CREATE TABLE link_settings (
  id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
  role            TEXT NOT NULL DEFAULT 'off' CHECK (role IN ('off', 'client', 'relay')),
  -- Both roles: the gateway's client certificate towards the link listener and
  -- the listener's own certificate (self-signed, pinned on the other side).
  gateway_key     TEXT,
  gateway_cert    TEXT,
  listener_key    TEXT,
  listener_cert   TEXT,
  -- client
  relay_addr      TEXT,
  relay_sni       TEXT,
  relay_ca_sha256 TEXT,
  client_key      TEXT,
  client_cert     TEXT,
  paired_at       TIMESTAMPTZ,
  -- relay
  link_host       TEXT,
  ca_key          TEXT,
  ca_cert         TEXT,
  server_key      TEXT,
  server_cert     TEXT,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- relay: the clients it accepts.
CREATE TABLE link_peers (
  id                   BIGSERIAL PRIMARY KEY,
  name                 TEXT NOT NULL UNIQUE,
  cert_sha256          TEXT NOT NULL UNIQUE,
  -- valid until the first connect with cert_sha256 (renewal)
  prev_cert_sha256     TEXT,
  cert_not_after       TIMESTAMPTZ NOT NULL,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_connected_at    TIMESTAMPTZ,
  last_disconnected_at TIMESTAMPTZ
);

-- relay: one-time pairing codes. A redeemed row keeps the CSR key's SHA-256
-- and the issued certificate (public), so a retry with the same key before
-- expiry gets the same certificate.
CREATE TABLE link_pairing_codes (
  id            BIGSERIAL PRIMARY KEY,
  peer_name     TEXT NOT NULL,
  secret_sha256 TEXT NOT NULL UNIQUE,
  expires_at    TIMESTAMPTZ NOT NULL,
  used_at       TIMESTAMPTZ,
  key_sha256    TEXT,
  issued_cert   TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- relay: hosts routed to a client (filled by stage 2). Created now so the
-- reservation of these hosts survives a rollback from stage 2.
CREATE TABLE link_ingress_hosts (
  host        TEXT PRIMARY KEY,
  peer_id     BIGINT NOT NULL REFERENCES link_peers(id) ON DELETE CASCADE,
  allowed_ips TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
