---
id: KRL-006
title: Show the Krill link on the topology map and the environment canvas
kind: feature
status: backlog
priority: 3
component: krill
depends_on: [KRL-005]
touches:
  - internal/topology/**
  - internal/server/topology_handlers.go
  - internal/server/env_pages.go
  - internal/server/system_handlers.go
  - internal/web/**
  - docs/internals/cluster.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

On the client install, the topology and the environment canvas show which apps
use the link, per service, so an operator and an org admin see the other
installation and the traffic through it without opening the Link page.

## Acceptance

- [ ] WHEN an organization has apps with egress or `via_relay` domains THEN
      `/orgs/{id}/topology` draws an external lane "link" beside the ingress lane
      with edges app ┄▶ relay ┄▶ destination (egress, labelled with the host) and
      internet ─▶ relay ─▶ gateway ─▶ app (ingress, labelled with the domain); the
      pure `internal/topology` package gains external nodes and still has no
      DB or Docker access.
- [ ] WHEN the environment canvas is drawn THEN app cards carry "↗ link" and
      "↘ relay" badges coloured by state; no new canvas wires.
- [ ] WHEN an org admin views either page THEN they see "link" and the state only;
      an instance operator also sees the relay's name. The relay's own topology
      never shows a client's services.
- [ ] WHEN docs are updated THEN `docs/internals/cluster.md` (Topology) describes the
      lane.

## Checks

- Unit tests of the graph builders with external nodes; render test that an org
  admin's page carries no relay address.
- `make generate`, `make test`; a look at both pages in light and dark themes.

## Limits / non-goals

- Display only; no new data from the link beyond KRL-002/005's state and manifest.

## Context

- `internal/topology` (`Build`, `BuildEnvView`, `LayoutCanvas`),
  `internal/server/topology_handlers.go` (`topoView`), `mountWiredMap` in
  `internal/web/static/app.js`.
