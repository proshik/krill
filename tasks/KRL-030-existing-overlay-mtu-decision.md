---
id: KRL-030
title: Decide whether Krill recreates existing overlay networks for a new MTU or keeps the manual procedure
kind: design
status: backlog
priority: 4
component: docs
depends_on: []
touches:
  - docs/architecture.md
harness: any
reviews: {spec: 1, code: 0}
created: "2026-11-27"
---

## Goal

`KRILL_OVERLAY_MTU` applies only to networks created after it is set. Docker cannot
change an existing network's MTU, and the gateway attaches networks by name, so an
existing network has to be recreated under the same name — today a documented
manual procedure with Krill stopped. The owner's decision between automating that
two-hop recreation and keeping the procedure has been pending since 2026-11-06; this
task records it.

## Acceptance

- [ ] WHEN the options are written THEN `docs/architecture.md#overlay-mtu` states for
      automation: the inventory of attached services (Krill's own and Traefik), the
      downtime per service and in total, the rollback if a step fails half-way, and
      how it interacts with the org-network migrator and a running deploy; and for the
      procedure: what it costs the operator and how often it is needed.
- [ ] WHEN the owner has chosen THEN the decision is recorded there with its date, and
      if it is automation, a feature task `KRL-<n>` is written with its acceptance.

## Checks

- One spec review round of the recorded options. No code.

## Limits / non-goals

- No implementation here.

## Context

- `docs/architecture.md#overlay-mtu` (the same-name two-hop procedure);
  `internal/config/config.go` (`KRILL_OVERLAY_MTU` is the container MTU, the network
  option is +50); `TestOverlayNetworkMTU`
  (`internal/docker/overlay_mtu_integration_test.go:21`).
- `docs/tasks/penpot-platform-follow-up.md` (stage S2: "implemented only after the
  owner chooses it over a documented procedure").
