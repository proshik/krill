---
id: KRL-001
title: Krill link core — pairing, mTLS + yamux transport and the relay endpoint behind Traefik
kind: feature
status: backlog
priority: 2
component: krill
depends_on: []
touches:
  - internal/link/**
  - internal/database/migrations/**
  - internal/database/queries/link*.sql
  - internal/database/gen/**
  - internal/panel/**
  - internal/traefik/**
  - internal/hostshare/**
  - internal/deploy/**
  - internal/server/**
  - internal/web/**
  - internal/config/**
  - cmd/krill/**
  - docs/internals/networking.md
  - docs/configuration.md
  - docs/guides/link.md
  - docs/install.md
  - go.mod
  - go.sum
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-27"
---

## Goal

Two Krill installations can be linked: a **client** (no direct reach to some foreign
APIs) holds one persistent outbound TLS connection to a **relay** abroad, and both
sides multiplex streams over it. This task builds the link itself — roles, pairing,
transport, reconnect, the relay endpoint and the operator page — with no traffic
forwarding yet (egress is KRL-002, ingress KRL-005). It is the base of the
"Krill link" feature: a general traffic redirect between two installations, with
the Telegram Bot API and LLM APIs as examples.

Decisions already made by the owner (do not reopen): transport is TLS 1.3 with
mutual authentication and a multiplexer inside the Krill binary (not SSH, not
WireGuard); roles are asymmetric (`off` | `client` | `relay`, one role per
install); a client has exactly one relay, a relay serves many clients; the link is
global infrastructure, gated by `RequireInstanceAdmin`.

**Release ordering:** this task must ship in an earlier release than KRL-002.
Self-update rolls back exactly one release, and the deploy rule below is what makes
a rollback from the egress release fail closed instead of sending apps direct.

## Acceptance

- [ ] WHEN the migration runs THEN it adds `link_settings` (singleton, `role` CHECK
      `off|client|relay`, key material columns), `link_peers` (`name` UNIQUE,
      `cert_sha256` UNIQUE, `prev_cert_sha256`, `cert_not_after`, connect/disconnect
      times), `link_pairing_codes` (`secret_sha256` UNIQUE, `expires_at`, `used_at`,
      `key_sha256`, `issued_cert`) and an empty `link_ingress_hosts (host PK, peer_id
      → link_peers ON DELETE CASCADE, allowed_ips TEXT NOT NULL DEFAULT '')` — filled
      only by KRL-005, created now so host reservations survive a rollback from
      stage 2. Additive only; `make lint-migrations` passes. Every private key is
      stored with `secret.Enc`; a pairing secret only as its SHA-256.
- [ ] WHEN any host is checked for reservation THEN one reserved-host check covers the
      panel domain, the link host and `link_ingress_hosts`, replacing `panelAvailable`
      (`internal/hostshare/store.go:281`, panel domain only) for every caller
      (`Add`, `ReservePrimaryHost`, …), under the `krill.domain-host:<host>` lock;
      `SetPanelDomain` checks the link hosts; the handlers' pre-check
      (`hostReservedByPanel`) gives the reason in the UI.
- [ ] WHEN an operator switches an install to `relay` and sets a link host THEN Krill
      creates a private link CA (ECDSA P-256) and a server certificate for the link
      host; the link host is validated like the panel domain (`panel.NormalizeHost`)
      and written through `hostshare` under the host lock, refused when a domain or
      the panel domain holds it. Without `KRILL_ADVERTISE_ADDR` or with a loopback
      listen address the role is refused with a stated reason; a panel domain is not
      required.
- [ ] WHEN the role is `relay` THEN `/_krill/gateway/config` merges the panel's config
      (`panel.DynamicConfig` is `{}` while the panel domain is off) with a `tcp`
      section: router `HostSNI(<link host>)` on `websecure`, `tls.passthrough`, to
      `KRILL_ADVERTISE_ADDR:<port of KRILL_LINK_LISTEN_ADDR>` through a
      `serversTransport` with `proxyProtocol.version=2` and an inline client
      certificate of the gateway; the panel's HTTP routers keep working. Before every
      publication the stored link hosts are re-checked against the panel domain and
      `domains` under the host lock (a release without the link could have handed one
      to a domain); a conflicting route is not published and the Link page shows both
      records until the operator resolves it.
- [ ] WHEN anything connects to `KRILL_LINK_LISTEN_ADDR` (new, default `:8445`,
      documented in `docs/configuration.md`) THEN Krill requires a PROXY v2 header
      followed by TLS in which Traefik presents the gateway client certificate, and
      closes the connection otherwise before reading the inner stream (a forged PROXY
      header from a container on the host must not be trusted). Under firewall
      lockdown the port is in the `Gateway` set (`controlPlaneServicePorts`) while the
      role is not `off`, never public. The lockdown ruleset drops every gateway port it
      does not list (`internal/firewall/ruleset.go:155-164`), so a role enabled under
      an existing lockdown leaves the port closed until the operator re-applies it:
      Krill never applies the firewall itself; the Link page says "re-apply the
      firewall" with a link, and the Firewall page's drift check shows it.
- [ ] WHEN the relay operator creates a pairing code for a peer name THEN the code
      `krl1_<base64url(JSON{addr, sni, ca_sha256, secret})>` is shown once (flash
      cookie), lives 10 minutes and is single-use; WHEN the client operator pastes it
      THEN the client generates its key, dials with TLS pinned to `ca_sha256`, opens
      one `pair` stream with the secret and a CSR, and stores the signed certificate
      (CN = peer name, 1 year). Redemption and issuance are one transaction: a
      conditional UPDATE (two attempts cannot both win) records the CSR key's SHA-256
      and the issued certificate on the code row and creates the peer; a retry with
      the **same key** before expiry gets the same certificate (the answer can be lost
      after the commit), any other key is refused. Pairing attempts are limited to
      10/min per source IP.
- [ ] WHEN a paired client runs THEN it keeps one session (TLS 1.3, client cert,
      `github.com/hashicorp/yamux`), sends `control` `hello {proto: 1,
      krill_version}`, reconnects with backoff 1–60 s with jitter; the relay checks the
      certificate fingerprint against `link_peers` on every handshake; a second session
      with the same certificate while the first lives is refused and shown as a
      duplicate; an incompatible `proto` is refused with a clear reason on both sides.
      Certificates are renewed over the live session at two thirds of their lifetime
      (`renew` → new cert, the previous fingerprint valid until the first connect with
      the new one).
- [ ] WHEN a peer is removed while one of its sessions is being admitted THEN the
      session cannot survive: admission and revocation are serialized per peer ID — the
      session is registered under the peer's lock after re-reading the peer by ID, the
      removal deletes the row and then closes the peer's sessions under the same lock,
      and every new stream checks its session is still registered.
- [ ] WHEN a stream opens THEN it starts with a header (1-byte type, 2-byte length,
      JSON) read within 5 s and at most 4 KB; unknown types reset the stream; at most
      1024 concurrent streams per peer; yamux keepalive 15 s, dead after 45 s.
- [ ] WHEN an app is redeployed THEN the deployer keeps every attachment the running
      service already has to a network named `krill-egress-*` (none exist before
      KRL-002; afterwards this is what keeps a rollback from the egress release fail
      closed), and `docs/install.md` Rollback says what a rollback below this release
      does to egress.
- [ ] WHEN an operator opens Settings → Link (`/orgs/{id}/link`,
      `RequireInstanceAdmin`, others get 404) THEN they can pick the role, see its
      requirements, create pairing codes and remove peers (relay), pair and forget
      the relay (client), and see state (connected / connecting / last error by
      category `dns|tcp|tls|unauthorized|incompatible|duplicate`), latency, since
      when, peer version and certificate expiry. All strings are `link.*` i18n keys
      in `en` and `ru`, lower case; destructive forms use `hx-boost="false"` +
      `data-confirm` + `krillConfirm`.
- [ ] WHEN anything is logged THEN it carries peer name, category and sizes, never
      keys, certificates, pairing codes or stream bytes.

## Checks

- Unit: pairing code encode/decode and expiry; retry with the same key / refusal with
  another; stream header limits; PROXY v2 parser (fuzz, truncated input); the
  reserved-host check from every caller; the admit/revoke race (peer removed between
  handshake and registration); dynamic config builder and the pre-publication
  re-check.
- Integration (`-tags=integration`): relay and client endpoints in one process over
  real TLS — pairing, code reuse refused, peer removal drops the session, duplicate
  refused, reconnect after relay restart; the relay gateway on real
  `traefik:v3.6.1` with the generated config: the link host passes through, the
  panel route still answers, a direct connection to the listener with a forged
  PROXY header and no gateway cert is refused; a redeploy keeps an existing
  `krill-egress-*` attachment.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- No egress or ingress forwarding, no destinations, no org grants (KRL-002, KRL-005).
- No API operations (KRL-004); no topology (KRL-006).
- No ALPN on the link; a DNS record for the link host is recommended, not required.
- The private link CA is the only trust root; no Let's Encrypt for the link host.

## Context

- Verified 2026-11-27 on `traefik:v3.6.1`: the HTTP provider accepts a `tcp` section;
  `HostSNI` passthrough lives beside HTTP routers on one entrypoint (TCP routers are
  tried first); a TCP `serversTransport` with PROXY v2 and an inline client
  certificate sends the PROXY header in clear, then TLS with that certificate, then
  the passed-through stream.
- `internal/panel/panel.go` (`DynamicConfig`, `Upstream`, `DeriveTokens`),
  `internal/traefik/bootstrap.go`, `internal/server/firewall_handlers.go:158`,
  `internal/hostshare/store.go`, `internal/deploy/deployer.go:1204`.
- The maintainer's design note `docs/superpowers/specs/2026-11-27-krill-link-design.md`
  (local-only; this task is authoritative without it).

## Implementation notes

Recorded by the implementing session (2026-11-27). Decisions marked *(impl)* are the
implementer's and reversible.

- **Touches widened, with the owner's consent (2026-11-27):** `internal/docker/**` —
  `ServiceSpec` cannot carry network aliases and the engine cannot read a service's
  attachments, so the deployer could not keep a `krill-egress-*` attachment with its alias
  (without the alias a rollback would fail open). The §18 lines for `AGENTS.md` (outside
  touches) are handed to the integrating session in the report; the detail lives in
  `docs/internals/networking.md`.
- *(impl)* yamux v0.1.2 has no public stream reset: an unknown stream type is closed
  without reading further (`Stream.Close`), which is what "reset" means here.
- *(impl)* Keepalive: `KeepAliveInterval=15s`, `ConnectionWriteTimeout=30s`, so an
  unanswered ping closes the session 45 s after the last answered one.
- *(impl)* Admission happens after the first stream: the relay reads `control hello`,
  looks the fingerprint up, admits under the peer lock and answers `hello` or
  `error{category}` — so the client learns `unauthorized`/`duplicate`/`incompatible`
  instead of a bare TLS alert. A duplicate attempt first pings the live session (5 s);
  a session that does not answer is closed and replaced.
- *(impl)* The gateway listener runs while the role is not `off`; on a `client` it has
  nothing to dispatch yet (egress is KRL-002) and closes authenticated connections.
- *(impl)* The gateway client cert and the listener cert are self-signed and pinned by
  SHA-256 on the other side (Traefik gets the listener cert as its root CA).
- *(impl)* The pairing form takes the address the client dials, default `<link host>:443`.

- *(impl, after review)* A client reconnects right after storing a renewed certificate (a
  second's break once a year) instead of renewing again in the same session: a session left
  on the old certificate could outlive both. Differs from spec §5.5 item 4 only in when the new
  certificate takes over.
- *(impl, after review)* A peer over the 1024-stream limit loses its whole session, not one
  stream: yamux v0.1.2 keeps a closed stream until the other side closes it, so refusing a
  single stream does not bound the peer.
- *(impl, after review)* Before admission a connection may send at most 64 KiB (the pairing
  side is reachable without a certificate and yamux buffers up to 256 KiB per unread stream).
- *(impl, after review)* `KRILL_LINK_LISTEN_ADDR` on a port the lockdown keeps public (22, 80,
  443, the UI port) refuses either role, and a stored role does not listen there at start.
- *(impl)* The relay's server certificate is reissued under the same CA 90 days before expiry
  (the spec gives it no renewal); the link CA lasts ten years.
- *(impl)* `firewall.GatewayOnly` ("some port is gateway-only") no longer means "the UI port is
  closed" once the link port is gateway-only too; the panel's drift check now asks for the UI
  port itself (`server.portGatewayOnly`). `firewall.GatewayOnly` is left without a production
  caller (`internal/firewall` is outside touches).

Plan: migration and queries → reserved-host check in `hostshare` → `internal/link`
primitives (PKI, pairing code, PROXY v2, stream header) → session registry → relay,
client, gateway listener, service → dynamic config and pre-publication check →
`docker` keep-prefix + deployer → handlers, page, i18n, firewall ports → wiring and
config → docs → integration tests. Lasting rules: `networking.md#krill-link`, plus the
AGENTS.md lines in the report.
