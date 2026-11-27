---
id: KRL-037
title: Decide whether OpenSearch fits as a managed engine with environment-scoped indices
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

OpenSearch as a managed engine was deferred in July 2026 because it needs the host's
`vm.max_map_count` raised and a JVM heap too heavy for the 1.9 GB VPS Krill targets.
This task decides whether it is offered (and on which nodes), and how
environment-scoped indices would map onto it, before any driver is written.

## Acceptance

- [ ] WHEN the decision is recorded THEN `docs/internals/databases.md` states the
      minimum node size and host settings, whether Krill sets `vm.max_map_count` or
      refuses a node without it, the security plugin and credential model, and how an
      environment gets its own indices and user (a future `logical_resources` or the
      bucket model) — or why the engine is dropped.
- [ ] WHEN the decision is to build it THEN a feature task is written.

## Checks

- One spec review round; a measurement of an idle single-node OpenSearch's memory on a
  local Swarm, recorded with the image tag.

## Limits / non-goals

- No driver here.

## Context

- `docs/internals/databases.md` (stateful services: "Deferred: OpenSearch …");
  `internal/dbservice/drivers` (the `Driver` interface).
