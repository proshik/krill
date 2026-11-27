---
id: KRL-013
title: Live acceptance of cluster-wide monitoring and the observability agent on a node added later
kind: research
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - docs/internals/operations.md
  - docs/internals/cluster.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The two monitoring paths that stop at node boundaries are confirmed on a real
cluster when a node joins: the Monitoring page's per-node CPU/memory (sampled over
an SSH tunnel to each worker's Docker socket) and the global Alloy agent service. The
2026-09-23 acceptance of the agent passed everything except a node added later (no
new node was planned on that cluster), and cluster-wide monitoring on two nodes was
"most likely already passed" without a record. This was ROADMAP's live-acceptance
items 3 and 12.

## Acceptance

- [ ] WHEN the Monitoring page is opened on a two-node cluster THEN both nodes have
      CPU and memory series, the node selector switches between them, and a stopped
      worker gets the "no data" badge after the freshness window, with gaps in chart
      buckets that have no samples. (The page still renders 0% CPU and zero used memory
      beside the stored capacity for such a node; a distinct "unreachable" state is
      not built — record it, a separate task if wanted.)
- [ ] WHEN a third node joins with the observability agent already on THEN an agent
      task starts on it without a settings change, its host metrics and container
      logs reach the receivers with `krill_*` labels (`krill_node` included), and the
      Observability page's coverage counts the new node.
- [ ] WHEN the node is drained and removed THEN its agent task goes away and the
      coverage stops waiting for it.

## Checks

- A dated record in `docs/internals/operations.md` (observability agent) and
  `docs/internals/cluster.md` (cluster-wide monitoring).

## Limits / non-goals

- The receivers belong to the operator; nothing about them is committed beyond
  neutral names.
- A failure becomes its own fix task.

## Context

- Agent: `internal/observability` (`reconcile.go`, coverage in
  `TestReconcileCoverageReportsLaggingNode`); sampler: `internal/metrics`.
- Already accepted on 2026-09-23 on a real two-node cluster: metrics and logs with
  `krill_*` labels in a real Prometheus/Mimir and Loki, agent memory under load and
  over a long window, Check against Mimir with authentication, a worker down or
  drained during a settings change, an authenticated receiver (the 0400 root-owned
  secret readable by Alloy).
