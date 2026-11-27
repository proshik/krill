---
id: KRL-044
title: Live acceptance of the per-organization network migration on a real cluster with services to move
kind: research
status: backlog
priority: 4
component: docs
depends_on: []
touches:
  - docs/internals/networking.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

`orgnet.Migrator` moves services that still run on the shared `KRILL_NETWORK` onto
their organization's `krill-org-<id>` network, databases before apps, without an
outage. It is proven by unit tests only. The path is not limited to installs older
than v0.1.0: the seeded `Default` organization is created without a network
(`SeedAdmin`, `internal/auth/service.go`), so anything deployed into it before the
first migration pass completes — or while the gateway reconcile keeps failing, which
skips the migration — runs on the shared network; at v0.1.0 a failed
`SetOrganizationNetwork` in `createOrg` also left an organization without its name.
This was ROADMAP's live-acceptance item 8.

## Acceptance

- [ ] WHEN a real two-node install has, in one organization, a running Postgres
      instance, a stopped database instance, a running image app linked to the
      database and a stopped app, all on the shared network, and Krill restarts THEN
      the database moves first and comes up, the running app is redeployed onto the
      organization network and keeps resolving the database, the stopped instance is
      parked (moved, not started) and the stopped app is skipped; the organization is
      marked migrated only after all of it finished.
- [ ] WHEN the pass runs THEN the gap each app sees (requests failing during its
      redeploy) is measured and recorded.
- [ ] WHEN one step fails (for example the linked database cannot start) THEN the
      organization stays unmarked and the next boot repeats the whole pass.

## Checks

- A dated record in `docs/internals/networking.md` (per-organization networks).
- How the services got onto the shared network for the test (an older release, or a
  hand-made service) is stated in the record.

## Limits / non-goals

- A disposable cluster; no production install.
- A failure becomes its own fix task.

## Context

- `internal/orgnet/migrate.go` (`Migrator`, two phases, 30-minute pass deadline,
  `finalSweepTimeout`), `orgnet.RunningInstancesFilter`, `dbservice.ParkInstance`;
  `cmd/krill/main.go` (the migrator starts only after the gateway reconcile succeeds).
- `docs/internals/networking.md#per-organization-networks`.
- Review of KRL-008 (revust r1, 2026-11-27) found the reachable paths above; the
  first version of the change record had dropped this item.
