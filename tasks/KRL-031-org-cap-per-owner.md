---
id: KRL-031
title: Cap how many organizations one user may create
kind: feature
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/org/**
  - internal/server/org_handlers.go
  - internal/server/org_handlers_test.go
  - internal/config/**
  - internal/web/i18n/**
  - docs/configuration.md
  - docs/internals/security.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

Any signed-in user can `POST /orgs` and own the new organization, and each new
organization changes the gateway's networks: Traefik is reconciled onto the new
`krill-org-<id>` network (debounced 3 s, at most once a minute), and because it has
host-mode ports that redeploy is StopFirst — a short ingress gap for every app. A
cap on organizations per user bounds how often one account can cause that.

## Acceptance

- [ ] WHEN a user who already created `KRILL_MAX_ORGS_PER_USER` organizations (counted
      by `organizations.owner_id`, the recorded creator, not by current `owner`
      memberships — an owner can promote someone else and demote themselves, which
      would free a slot while the organization and its network stay) creates another
      THEN it is refused with a flash that says so, before the row or the
      network is created; the instance operator (`users.is_admin`) is not capped.
- [ ] WHEN the variable is unset THEN the default does not refuse anything an existing
      install already has (a user over the cap keeps their organizations and only
      cannot create more); `0` means unlimited. The default value is the spec's choice,
      recorded in `docs/configuration.md` with the reason.
- [ ] WHEN roles in an organization change THEN the creator's count is unchanged.
- [ ] WHEN two creations race THEN the cap is enforced in one transaction (or under a
      per-user lock), not by a check-then-insert that both pass.
- [ ] WHEN the refusal happens THEN it is logged at INFO with `user_id` via `logFrom(r)`.

## Checks

- Unit/handler: under, at and over the cap; operator exempt; `0` unlimited; the race
  (two concurrent POSTs at cap − 1 → one succeeds).
- `make test`.

## Limits / non-goals

- Counts organizations by their recorded creator (`owner_id`); role changes and
  memberships granted by others do not change the count. A test changes roles and
  checks the quota is unchanged.
- No billing, no per-organization quotas on apps.

## Context

- `internal/server/org_handlers.go:58` (`createOrg`: `CreateOrg` → `NetworkEnsure` →
  rollback of an org left without a network → gateway reconcile).
- `docs/internals/networking.md#per-organization-networks` (`Server.reconcileGateway`,
  debounced 3 s, one reconcile a minute); StopFirst for host-mode ports:
  `docs/internals/networking.md`.
- Config lives in `internal/config/config.go`; every variable is listed in
  `docs/configuration.md`.
