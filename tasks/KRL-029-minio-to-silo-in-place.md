---
id: KRL-029
title: Verify and document moving an existing MinIO instance onto Silo in place
kind: research
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - docs/guides/buckets.md
  - docs/internals/databases.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

S3 servers created before v0.7.0 keep their pinned `quay.io/minio/minio` release,
which receives no updates (and answers 401 to anonymous pulls). New servers run
Silo (`drivers.MinIOImage`). An operator gets a verified, documented way to move an
old instance onto Silo without losing objects, users or policies — or a documented
reason why it cannot be done in place.

## Acceptance

- [ ] WHEN a MinIO instance with buckets, users, policies and objects (created with
      the last MinIO image Krill pinned) has its image changed to
      `drivers.MinIOImage` through the existing version action (`versionDBInstance`,
      which accepts any image) THEN Silo starts on the same volume and every object,
      user and policy is still there, and app links keep working — or the
      incompatibility is recorded with the error.
- [ ] WHEN the move works THEN `docs/guides/buckets.md` has the steps (back up the
      buckets first, the image to set, what to check) and the rollback (the old image
      back on the same volume), and `docs/internals/databases.md` records the check.
- [ ] WHEN the move does not work in place THEN the guide describes and the check
      verifies a cross-server path — Krill's **Restore into a new bucket** creates the
      bucket on the snapshot's own server, so it cannot move data to a new Silo server
      by itself: e.g. a new Silo server, the bucket and its key created or imported there,
      the objects copied with `mc mirror` between the two servers (explicit source and
      destination, no root credential in an app), then each app relinked and checked —
      and the task records why the in-place move failed.

## Checks

- A local Swarm run with the old image on a volume, then Silo on the same volume;
  if the old image cannot be pulled anonymously, note the source used. Optionally an
  integration test pinning the result.

## Limits / non-goals

- No automatic migration in Krill; the UI wording is changed only if the guide
  needs a pointer.

## Context

- `internal/dbservice/drivers/minio.go:22-33` (`DefaultImage`, `MinIOImage`); the
  image is stored per instance (`db_instances.image`, rewritten to `quay.io` by
  migration 000044).
- `internal/server/db_instance_handlers.go:462` (`versionDBInstance`).
- `docs/internals/databases.md` (MinIO driver: "existing instances keep their image").
