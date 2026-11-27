---
id: KRL-012
title: Live acceptance of moving a database instance's volume between nodes, including a failure mid-transfer
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/internals/databases.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

Changing a database instance's node on a real cluster moves its data volume and
brings the instance up on the target, and a transfer that fails half-way leaves the
instance running on the source with its data intact. This was ROADMAP's
live-acceptance item 4; the path is code-complete and unit-tested only.

## Acceptance

- [ ] WHEN a Postgres instance with a known data set is moved from the control plane
      to a worker (and back) THEN it comes up on the target with the same data, its
      apps reconnect, and the transfer time for the volume size is recorded.
- [ ] WHEN the target node is cut off (network down or Docker stopped) in the middle
      of the stream THEN the move fails, the instance is redeployed on the source with
      its data, the error says why, and a retry after the node returns succeeds.
- [ ] WHEN a second move or a Deploy is started while one runs THEN it is refused
      (`ErrInstanceBusy` / `ErrDeployInProgress`) with a flash that says so, not a
      false "migration failed" alert.
- [ ] WHEN the move takes longer than `KRILL_MIGRATE_TIMEOUT` THEN it is rolled back
      to the source as on any other failure.
- [ ] WHEN "delete source" is not chosen THEN the source volume is still there after a
      successful move.

## Checks

- A dated record in `docs/internals/databases.md` (DB instances: node moves), with
  the volume size, the duration and what the failure looked like.

## Limits / non-goals

- A failure becomes its own fix task.

## Context

- `internal/dbservice/migrate.go:40` (`MigrateInstanceNode`: stop → stream →
  redeploy on the target, rollback to the source on any error; takes the instance
  lock before returning).
- `docs/internals/databases.md` (DB instances).
