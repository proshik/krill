---
id: KRL-004
title: Operator token level and agent API operations for the Krill link (stage 1)
kind: feature
status: backlog
priority: 2
component: krill
depends_on: [KRL-002]
touches:
  - internal/api/**
  - internal/mcpsrv/**
  - internal/server/**
  - internal/web/**
  - internal/database/migrations/**
  - internal/database/gen/**
  - docs/internals/agent-api.md
  - docs/internals/security.md
  - skills/krill-deploy/**
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-27"
---

## Goal

Everything the link offers in the UI can be done through `/api/v1` and `/mcp`.
The link is instance-level infrastructure, while agent API tokens are scoped to one
organization. The owner chose one more step on the existing level ladder instead
of a second kind of token, so the token used for everyday deploys can also
configure the link.

## Acceptance

- [ ] WHEN the migration runs THEN `api_tokens_level_check` is recreated wider with
      `'operator'` (the 000063 pattern); the previous release, which compares levels
      with `write`/`provision` only, treats an `operator` token as read-only — a
      unit test pins that an unknown level gains nothing.
- [ ] WHEN a token has level `operator` THEN it can do everything `provision` can in
      its organization, plus the link operations below, as long as its owner is
      live org admin AND live `users.is_admin` (re-read on every request, like the
      role today; `Identity` gains the flag). Only an instance operator can mint it:
      the level is not offered to others and the server answers 400. The token page
      says in one line that the link operations it allows are instance-wide.
- [ ] WHEN called with an `operator` token THEN these work, idempotent (desired state
      in, repeat = no change), identical over REST and MCP:
      `krill_link_status` / `GET /api/v1/link`;
      `krill_link_set_role` / `PUT /link/role {role, link_host?}`;
      `krill_link_pairing_code` / `POST /link/pairing-codes {peer_name}` →
      `{code, expires_at}` (relay);
      `krill_link_pair` / `POST /link/pair {code}` (client);
      `krill_link_remove_peer` / `POST /link/peers/{name}/remove` (relay);
      `krill_link_unpair` / `POST /link/unpair` (client);
      `krill_link_set_destinations` / `PUT /link/destinations {destinations}`
      (relay — the single destination list);
      `krill_link_set_org_grants` / `PUT /link/org-grants {orgs}` (client).
- [ ] WHEN called with a `read`/`write` token of a granted organization THEN
      `krill_app_link` / `GET /apps/{app}/link` (read) and `krill_app_set_egress` /
      `PUT /apps/{app}/egress {destinations}` → `{pending}` (write) work; the
      change applies through the existing `krill_deploy`; outside a granted
      organization they are 404, cross-org 404. App verbs are one segment (the router
      peels one, `internal/server/api_handlers.go:127-145`) and `link` and `egress` go
      into `refShapedVerbs`, so `GET /api/v1/apps/project/prod/link` — today the status
      of an app named `link` — keeps working (a test pins it).
- [ ] WHEN status is returned THEN it carries fingerprints and expiry dates, never
      keys or whole certificates, and errors by category without resolver
      addresses; the pairing code is returned only by `krill_link_pairing_code`; the
      role is in `krill_link_status` (`krill_whoami` is unchanged).
- [ ] WHEN docs are updated THEN `docs/internals/agent-api.md` lists the level and
      the operations and records that remove-peer, unpair and narrowing a list are
      the first operations of the agent API that remove configuration (operator
      level only); `docs/internals/security.md` gets the level;
      `skills/krill-deploy/SKILL.md` gets a link runbook.

## Checks

- Unit: the level × role × `is_admin` matrix for every operation; demotion and loss
  of `is_admin` take effect on the next request (MCP sessions included, per
  `callerIdentity`); MCP tool schemas stay flat (`TestProvisionToolSchemasAreFlat`).
- Integration: REST and MCP against the in-process link endpoints of KRL-001/002.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- Stage 2 operations (ingress) are part of KRL-005.
- No `krill-cli` commands for the link here.

## Context

- `internal/api/identity.go:25-56` (levels, `CanWrite`, `CanProvision`),
  `internal/database/migrations/000063_api_token_provision.up.sql`,
  `internal/server/api_token_handlers.go`.
- Whatever an agent reads over MCP reaches its model provider: the pairing code is
  single-use, expires in 10 minutes and is useless once redeemed.
