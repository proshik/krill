---
id: KRL-038
title: Decide how Krill encrypts traffic between nodes
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

Overlay traffic between nodes (VXLAN) is unencrypted unless the operator runs the
swarm over their own WireGuard. Krill already cooperates with an operator's
WireGuard (it admits the listen ports, binds swarm ports to the interface and sizes
the MTU) but never sets it up. This task decides what Krill offers.

## Acceptance

- [ ] WHEN the options are recorded THEN `docs/architecture.md` compares at least:
      Docker's encrypted overlay (`--opt encrypted`, IPsec per network — MTU cost,
      kernel requirements, whether existing networks must be recreated, cf. KRL-030);
      WireGuard set up by Krill at node join (keys, address plan, rotation, what happens
      to a running cluster); and documenting an operator-run WireGuard as the supported
      way.
- [ ] WHEN the choice is made THEN it is recorded with its date, and implementation, if
      any, becomes a feature task.

## Checks

- One spec review round. Optionally a local measurement of throughput and MTU with
  `--opt encrypted` on a two-node test swarm.

## Limits / non-goals

- No implementation here.

## Context

- `internal/firewall/probe.go` (WireGuard interfaces and ports), `internal/orgnet`
  (network creation), `docs/architecture.md#overlay-mtu`, `docs/internals/cluster.md`.
