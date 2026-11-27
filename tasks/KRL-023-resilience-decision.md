---
id: KRL-023
title: Decide how Krill survives the loss of the manager — a resilient ingress, and HA or a fast-recovery runbook
kind: design
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/architecture.md
harness: any
reviews: {spec: 1, code: 0}
created: "2026-11-27"
---

## Goal

Today one manager node carries both the control plane and all ingress: Krill is a
single host process with no high availability, and Traefik is one replica pinned
to the manager with host-mode ports 80/443. If the manager goes down, the UI and
every app's traffic go with it. This task records a decision on what Krill does
about it, so that the work that follows (and the later federation idea, KRL-034)
builds on a chosen direction.

## Acceptance

- [ ] WHEN the decision is recorded THEN `docs/architecture.md` has a section that
      states, for the ingress and for the control plane separately, the option chosen
      and the ones rejected with their cost: e.g. Traefik as a global service on
      several nodes with DNS or a floating address in front, versus one replica
      rescheduled on failure; a second control-plane process with leader election
      versus a documented, timed recovery onto a new host from the state backup.
- [ ] WHEN the ingress option needs certificates on several nodes THEN the decision
      says where the ACME state lives (today the `krill-traefik-acme` volume on the
      manager) and how two Traefik replicas avoid racing for the same certificate.
- [ ] WHEN the decision is a runbook rather than HA THEN it names a target recovery
      time and what it is measured on (the state-backup runbook A in
      `docs/install.md` is the starting point).
- [ ] WHEN the decision is made THEN the implementation work is written as new tasks
      (`KRL-<n>`), and KRL-034's `depends_on` still points here.

## Checks

- One spec review round (`reviews.spec: 1`) of the recorded decision. No code.

## Limits / non-goals

- No implementation; no change to `internal/traefik` or the deploy path here.
- The budget stays the one Krill is built for (a 2 vCPU / 1.9 GB control-plane VPS):
  any option's memory and node count are stated.

## Context

- `docs/architecture.md` ("The control plane is a single process on the manager
  node; it has no high availability").
- `internal/traefik/bootstrap.go:181-196` (`Replicas: 1`, host-mode 80/443,
  `node.role==manager`, the `krill-traefik-acme` volume); a service with a host-mode
  port deploys StopFirst (`buildSwarmSpec`), so every gateway spec change is a short
  gap.
- Krill state backup and its restore runbooks: `docs/internals/operations.md`,
  `docs/install.md`.
