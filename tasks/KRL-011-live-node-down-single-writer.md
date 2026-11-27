---
id: KRL-011
title: Live acceptance of node-down reconciliation and the database single-writer guard on two nodes
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/internals/databases.md
  - docs/architecture.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

A drained, removed or returning worker on a real cluster never leaves two database
processes writing one volume. On 2026-09-18 a returning worker produced two Postgres
postmasters on the same volume (overlapping about 0.3 s, then the new one serving
for about 60 s on a directory the old one had already shut down, until Postgres's
once-a-minute lock-file recheck stopped it). The fix — stop-first updates and a
portable directory lock around the entrypoint — is proven in tests only. This was
ROADMAP's live-acceptance item 2.

## Acceptance

- [ ] WHEN a worker holding a Postgres instance is drained and then returned in the
      middle of reconciliation THEN the second container's postmaster stays blocked
      on the lock until the first container is gone, and its log has no "was not
      properly shut down" line once it starts (no crash recovery: the two never
      overlapped).
- [ ] WHEN the same is done with a Redis, DragonFly and Valkey instance THEN the
      second process starts only after the first is gone.
- [ ] WHEN a worker is removed from the cluster THEN its services are rescheduled or
      reported as the Nodes page and the environment canvas say, with no lost write
      on a database that moved.
- [ ] WHEN an instance was only Started/Stopped since the fix shipped (never
      redeployed) THEN it is confirmed still unprotected — Start and Stop only scale
      the old spec — and the docs say it needs one explicit Deploy.

## Checks

- A dated record in `docs/internals/databases.md` (single-writer protection) with the
  container timelines from `docker service ps` and the Postgres log lines.

## Limits / non-goals

- Two real nodes; nothing on a production install without its owner's window.
- A failure becomes its own fix task.

## Context

- `internal/dbservice/drivers/driver.go:104` (the `sh -c 'exec 9<"$0" && flock 9 &&
  exec …'` wrapper; never `flock --no-fork`, busybox lacks it); every driver sets
  `UpdateStopFirst: true`; MinIO/Silo has no `flock` (stop-first only).
- Node availability: `internal/server/node_avail.go`; nodes in
  `docs/architecture.md#nodes`, `docs/internals/cluster.md`.
