---
id: KRL-005
title: Krill link ingress — an app host served through the relay with TLS passthrough to the client
kind: feature
status: backlog
priority: 2
component: krill
depends_on: [KRL-002, KRL-004]
touches:
  - internal/link/**
  - internal/traefik/**
  - internal/panel/**
  - internal/hostshare/**
  - internal/deploy/**
  - internal/api/**
  - internal/mcpsrv/**
  - internal/server/**
  - internal/web/**
  - internal/database/migrations/**
  - internal/database/queries/**
  - internal/database/gen/**
  - docs/internals/networking.md
  - docs/internals/agent-api.md
  - docs/guides/link.md
  - docs/guides/domains.md
  - docs/install.md
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-27"
---

## Goal

A webhook (Telegram, Stripe, GitHub, anything over HTTPS) reaches an app on the
client install: the host's DNS points at the relay, the relay passes the TLS
connection through the link without decrypting it, and the client's Traefik
terminates TLS with its own certificate. The relay never sees plaintext or a
webhook's `secret_token` (owner decision: passthrough, certificate on the client).

## Acceptance

- [ ] WHEN the migration runs THEN it adds `domains.via_relay BOOLEAN NOT NULL DEFAULT
      false`, additive only (`link_ingress_hosts` already exists from KRL-001).
- [ ] WHEN the relay operator adds an ingress host for a peer THEN it is validated like
      a domain and written through `hostshare` under the host lock, refused when the
      relay's panel domain, an app domain or the link host holds it (the reserved-host
      check of KRL-001 already reads this table, so it binds in both directions); an
      optional IP allowlist is CIDR/IP, canonicalized, entered by hand (the guide
      points to Telegram's documented webhook subnets,
      https://core.telegram.org/bots/webhooks, instead of copying them). The
      relay sends each peer the hosts it owns in `control`.
- [ ] WHEN an ingress host exists THEN the relay's `/_krill/gateway/config` routes
      `HostSNI(host)` on `websecure` with passthrough and **no middleware** to the
      authenticated gateway listener (PROXY v2 + gateway client cert, KRL-001); Krill
      reads the SNI and the ALPN list from the ClientHello, finds the peer (none or no
      session → close) and applies the host's allowlist itself to the PROXY v2 source
      address — except a connection whose ALPN is exactly `[acme-tls/1]` (RFC 8737's
      validator), which passes so Let's Encrypt can validate and renew while the
      allowlist is on. Traefik's TCP `ipAllowList` is not used: it would also drop the
      validator, and Traefik v3.6.1 refuses an ``ALPN(`acme-tls/1`)`` rule. Then Krill
      opens an `ingress` stream `{host, src}`; the client IP goes to the relay's log.
- [ ] WHEN the client receives an `ingress` stream THEN it accepts it only for its own
      host with `via_relay`, dials its gateway at `KRILL_ADVERTISE_ADDR:443` and joins
      the streams.
- [ ] WHEN `traefik.TraefikSpec` is built for the role `client` THEN it carries a second
      resolver `le-alpn` (`tlschallenge`, its own storage file in the
      `krill-traefik-acme` volume); for any other role it does not — a relay with a
      TLS-ALPN resolver answers `acme-tls/1` itself and breaks the passthrough. A role
      change into or out of `client` redeploys the gateway once (StopFirst), and the
      Link page warns before it. `traefik.AppLabels` uses `le-alpn` instead of `le`
      for a `via_relay` host; the flag travels through `deploy.TraefikDomains`.
- [ ] WHEN an org admin of a granted organization turns on "receive through relay"
      THEN it applies to the whole host, like TLS (`hostshare.SetTLS`): a new
      `hostshare.SetViaRelay` sets `domains.via_relay` on every row of the host under
      the host lock; the card warns when other apps share the host; it is accepted
      only for an exposed TLS host the relay sent to this peer; the card shows the
      state (relay accepts / host not allowed by relay / no session).
- [ ] WHEN a `via_relay` host has its own `domains.allowed_ips` THEN it keeps applying
      on the client to both the direct path (`:443` is published in host mode) and the
      tunnel, and `AppLabels` adds the address the client's Traefik sees Krill's
      connections from (pinned by the integration test); the relay's allowlist is an
      extra filter on the relay path only, and the card and the guide say so. The
      domain's allowlist field stays visible.
- [ ] WHEN the agent API is used THEN `krill_link_set_ingress_hosts` /
      `PUT /api/v1/link/ingress-hosts {hosts: [{host, peer, allowed_ips}]}`
      (operator, relay) and `krill_domain_set_via_relay` /
      `PUT /api/v1/apps/{app}/via-relay {host, enabled}` (write, granted
      organization; `via-relay` is added to `refShapedVerbs`) work like the stage 1
      operations.
- [ ] WHEN docs are updated THEN networking.md, the domains and link guides describe
      the path, both allowlists and the direct path, and `docs/install.md` Rollback
      says that a release without the link writes `le` again, so a renewal of such a
      host fails (90-day certificate) until the release is back.

## Checks

- Unit: relay dynamic config (no middleware); the ingress allowlist decision by source
  address and ALPN (`[acme-tls/1]` only passes; `[acme-tls/1, h2]` does not);
  `TraefikSpec` args per role; `AppLabels` resolver choice and the Krill address in
  the allowlist; `SetViaRelay` on a shared host.
- Integration on real `traefik:v3.6.1`: the relay gateway passes an ingress host
  through; `acme-tls/1` with that SNI reaches the backend through the relay (no
  TLS-ALPN resolver there); the client's Traefik with `le-alpn` serves an
  `acme-tls/1` connection itself and never routes it to an HTTP router; the panel and
  app HTTP routes still answer; an end-to-end ingress stream through two in-process
  link endpoints to a local Traefik; the address Traefik sees for Krill's dial is
  recorded and allowed.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- No HTTP-01 and no `:80` through the relay: verified 2026-11-27, the relay's
  built-in ACME router answers 404 to a foreign host's challenge path, and
  `allowACMEByPass` would hand the relay's own challenges to its app routers.
- No TLS termination on the relay; no client IP for the app (PROXY v2 into the
  client's Traefik is deferred).
- Real Let's Encrypt issuance and renewal over TLS-ALPN-01 are checked in KRL-007.

## Context

- Verified 2026-11-27 on `traefik:v3.6.1`: `acme-tls/1` through a passthrough
  `HostSNI` route reaches the backend when the relay has only an HTTP-01 resolver,
  and is answered by the relay itself (`unrecognized name`) once a `tlschallenge`
  resolver is added; ``ALPN(`acme-tls/1`)`` in a TCP rule is rejected ("not
  allowed"); Traefik starts with two resolvers.
- `internal/traefik/labels.go:174,185,229`, `internal/traefik/bootstrap.go`
  (`TraefikSpec`, host-mode `:443`), `internal/hostshare/store.go:140` (`SetTLS`),
  `internal/deploy/store.go` (`TraefikDomains`), `internal/panel/panel.go`.
