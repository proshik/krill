-- name: InitLinkSettings :exec
INSERT INTO link_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING;

-- name: GetLinkSettings :one
SELECT * FROM link_settings WHERE id;

-- name: LockLinkSettings :one
SELECT * FROM link_settings WHERE id FOR UPDATE;

-- name: SetLinkGatewayMaterial :exec
UPDATE link_settings
SET gateway_key = $1, gateway_cert = $2, listener_key = $3, listener_cert = $4, updated_at = now()
WHERE id;

-- name: SetLinkRoleOff :exec
UPDATE link_settings SET role = 'off', updated_at = now() WHERE id;

-- name: SetLinkRoleClient :exec
UPDATE link_settings SET role = 'client', updated_at = now() WHERE id;

-- name: SetLinkRelay :exec
UPDATE link_settings
SET role = 'relay', link_host = $1, ca_key = $2, ca_cert = $3, server_key = $4, server_cert = $5, updated_at = now()
WHERE id;

-- name: SetLinkClientPaired :exec
UPDATE link_settings
SET role = 'client', relay_addr = $1, relay_sni = $2, relay_ca_sha256 = $3,
    client_key = $4, client_cert = $5, paired_at = now(), updated_at = now()
WHERE id;

-- name: SetLinkClientCert :execrows
UPDATE link_settings SET client_key = $1, client_cert = $2, updated_at = now()
WHERE id AND client_cert = sqlc.arg(old_cert)::text;

-- name: ForgetLinkRelay :exec
UPDATE link_settings
SET relay_addr = NULL, relay_sni = NULL, relay_ca_sha256 = NULL,
    client_key = NULL, client_cert = NULL, paired_at = NULL, updated_at = now()
WHERE id;

-- name: GetLinkRelayHost :one
-- The link host a relay routes, "" when the installation is not a relay.
SELECT COALESCE(link_host, '')::text FROM link_settings WHERE id AND role = 'relay';

-- name: CountLinkIngressHostsByHost :one
-- A relay ingress host (stage 2), kept by a rollback.
SELECT count(*) FROM link_ingress_hosts WHERE lower(host) = lower(sqlc.arg(host)::text);

-- name: ListLinkIngressHosts :many
SELECT host FROM link_ingress_hosts ORDER BY host;

-- name: ListLinkPeers :many
SELECT * FROM link_peers ORDER BY name;

-- name: GetLinkPeer :one
SELECT * FROM link_peers WHERE id = $1;

-- name: GetLinkPeerByFingerprint :one
SELECT * FROM link_peers WHERE cert_sha256 = $1 OR prev_cert_sha256 = $1;

-- name: CreateLinkPeer :one
INSERT INTO link_peers (name, cert_sha256, cert_not_after) VALUES ($1, $2, $3)
RETURNING *;

-- name: DeleteLinkPeer :execrows
DELETE FROM link_peers WHERE id = $1;

-- name: MarkLinkPeerConnected :exec
-- A connect with the current certificate retires the previous one.
UPDATE link_peers
SET last_connected_at = now(),
    prev_cert_sha256 = CASE WHEN cert_sha256 = sqlc.arg(fingerprint)::text THEN NULL ELSE prev_cert_sha256 END
WHERE id = $1;

-- name: MarkLinkPeerDisconnected :exec
UPDATE link_peers SET last_disconnected_at = now() WHERE id = $1;

-- name: RenewLinkPeerCert :execrows
-- The certificate the session holds stays valid as prev_cert_sha256 until the
-- first connect with the new one. A session still on the previous certificate
-- may renew again (its first answer was lost): it keeps its own as previous.
UPDATE link_peers
SET prev_cert_sha256 = sqlc.arg(session_fingerprint)::text,
    cert_sha256 = sqlc.arg(new_fingerprint)::text, cert_not_after = $2
WHERE id = $1 AND (cert_sha256 = sqlc.arg(session_fingerprint)::text
                   OR prev_cert_sha256 = sqlc.arg(session_fingerprint)::text);

-- name: CreateLinkPairingCode :one
-- The database clock decides expiry, here and on redemption.
INSERT INTO link_pairing_codes (peer_name, secret_sha256, expires_at)
VALUES ($1, $2, now() + make_interval(secs => sqlc.arg(lifetime_seconds)::float8))
RETURNING *;

-- name: LockLinkPairingCode :one
SELECT id, peer_name, used_at, key_sha256, issued_cert
FROM link_pairing_codes WHERE secret_sha256 = $1 FOR UPDATE;

-- name: LinkPairingCodeLive :one
-- Asked after LockLinkPairingCode holds the row: the lock may be granted
-- after the code expired, and an expression in the locking query itself is
-- evaluated before the wait. clock_timestamp(), not now(), which is the start
-- of the transaction.
SELECT (expires_at > clock_timestamp())::boolean FROM link_pairing_codes WHERE id = $1;

-- name: RedeemLinkPairingCode :execrows
-- Two redemptions of one code cannot both win: only an unused, unexpired row
-- is updated.
UPDATE link_pairing_codes
SET used_at = now(), key_sha256 = $2, issued_cert = $3
WHERE secret_sha256 = $1 AND used_at IS NULL AND expires_at > clock_timestamp();

-- name: ListOpenLinkPairingCodes :many
SELECT id, peer_name, expires_at FROM link_pairing_codes
WHERE used_at IS NULL AND expires_at > now() ORDER BY created_at;

-- name: DeleteStaleLinkPairingCodes :exec
DELETE FROM link_pairing_codes WHERE expires_at < now() - interval '1 day';

-- name: CountLinkPeersByName :one
SELECT count(*) FROM link_peers WHERE name = $1;

-- name: ListLinkHostDomainHolders :many
-- Every application domain sitting on a host the link wants, with where to
-- find it.
SELECT a.id AS app_id, a.name AS app_name, a.environment_id, e.name AS env_name,
       p.id AS project_id, p.name AS project_name, p.organization_id AS org_id
FROM domains d
JOIN applications a ON a.id = d.application_id
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
WHERE lower(d.host) = lower(sqlc.arg(host)::text)
ORDER BY a.id;

-- name: CountLinkHostDomains :one
-- Domain rows on a host, whatever their letter case: a link host is compared
-- with app domains as DNS compares names.
SELECT count(*) FROM domains WHERE lower(host) = lower(sqlc.arg(host)::text);

-- name: SetLinkServerCert :execrows
-- Reissue the relay's server certificate only while the settings still name
-- this relay and host.
UPDATE link_settings SET server_key = $1, server_cert = $2, updated_at = now()
WHERE id AND role = 'relay' AND link_host = sqlc.arg(host)::text;
