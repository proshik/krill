---
id: KRL-039
title: Decide whether managed Postgres gets replicas and failover
kind: design
status: backlog
priority: 4
component: docs
depends_on: []
touches:
  - docs/internals/databases.md
harness: any
reviews: {spec: 1, code: 0}
created: "2026-11-27"
---

## Goal

A managed Postgres instance is one Swarm service with one volume on one node; a lost
node means a restore from backup. This task decides whether Krill offers streaming
replicas (read-only, or with failover) and how that coexists with the single-writer
guarantees, before any code.

## Acceptance

- [ ] WHEN the design is recorded THEN `docs/internals/databases.md` states the model
      (a replica as a second instance or a property of one), how replication users and
      slots are managed, how a failover is triggered (manual first?) and how apps'
      links follow the primary, and how the stop-first + directory-lock guarantee is
      kept for the primary — or why replication is dropped in favor of backups.
- [ ] WHEN the decision is to build it THEN feature tasks are written.

## Checks

- One spec review round. No code.

## Limits / non-goals

- No implementation; Redis-family replication is out of scope unless the design
  includes it explicitly.

## Context

- `internal/dbservice` (instances, `MigrateInstanceNode`), single-writer protection in
  `docs/internals/databases.md` and AGENTS.md "Managed databases".
