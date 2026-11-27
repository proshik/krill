---
id: KRL-034
title: Decide whether and how Krill federates installs across data centers with GeoDNS for disaster recovery
kind: design
status: backlog
priority: 4
component: docs
depends_on: [KRL-023]
touches:
  - docs/architecture.md
harness: any
reviews: {spec: 1, code: 0}
created: "2026-11-27"
---

## Goal

A later idea from ROADMAP: two or more Krill installs in different data centers,
with GeoDNS sending users to a healthy one, as disaster recovery. It is strictly
after the single-site resilience decision (KRL-023), because federation multiplies
whatever single-site recovery Krill has.

## Acceptance

- [ ] WHEN the design is recorded THEN `docs/architecture.md` states what is
      replicated between installs (Krill's own state, app images, database data,
      buckets, certificates), in which direction, how a failover and a failback happen,
      and what an app must do to be federated — or why the idea is dropped.
- [ ] WHEN the design relies on the Krill link (KRL-001…KRL-007) or on state backups
      THEN it says which parts it reuses.
- [ ] WHEN the decision is to build it THEN the work is written as new tasks.

## Checks

- One spec review round. No code.

## Limits / non-goals

- No implementation; no choice of a DNS provider in code.

## Context

- KRL-023 (single-site resilience); `docs/architecture.md`; state backups:
  `docs/internals/operations.md`.
