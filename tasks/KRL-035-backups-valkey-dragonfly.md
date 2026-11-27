---
id: KRL-035
title: Scheduled backups to S3 for Valkey and DragonFly database servers
kind: feature
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/backup/**
  - internal/dbservice/**
  - internal/server/**
  - internal/web/**
  - internal/notify/**
  - internal/database/migrations/**
  - internal/database/queries/**
  - internal/database/gen/**
  - cmd/krill/**
  - docs/guides/backups.md
  - docs/internals/databases.md
harness: any
reviews: {spec: 1, code: 2}
created: "2026-11-27"
---

## Goal

Database backups exist only for Postgres logical databases (`backups.logical_database_id`);
volume backups cover app volumes and bucket snapshots cover S3. A Valkey or
DragonFly server — often holding queues or sessions that matter — has no backup.
This task gives those instance-level servers the same schedule, retention, restore
and alerts.

## Acceptance

- [ ] WHEN an org admin configures a backup for a Valkey or DragonFly instance THEN it
      runs on the same schedule presets, storage and retention as a Postgres backup,
      under its own S3 directory, and is counted in the self-update busy check.
- [ ] WHEN a backup runs THEN the snapshot is consistent: the engine's own snapshot
      (`BGSAVE` / DragonFly's `SAVE` to its snapshot format, waited for until done) is
      streamed out of the container through `Engine.Exec`, never by copying files of a
      live process; the superuser password reaches the process over stdin or a file,
      never argv (`Engine.Exec`'s error text embeds argv).
- [ ] WHEN a restore runs THEN the instance is stopped first, the snapshot replaces the
      data, the instance comes back, and keys written after the snapshot are gone (the
      UI says so before confirming).
- [ ] WHEN a backup fails THEN the alert resolves its owner through this backup kind's
      own table (separate id sequences — see `notify.Service.BucketBackupFailed`).
- [ ] WHEN the migration runs THEN it is additive (a new table or a nullable target
      column), and the previous release ignores the new rows.
- [ ] WHEN the docs are updated THEN `docs/guides/backups.md` and
      `docs/internals/databases.md` describe it.

## Checks

- Integration (`-tags=integration`) on the real Valkey and DragonFly images: write keys,
  back up, write more, restore → exactly the first set.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- Redis instances may come along if they share the code path; MinIO/Silo is covered by
  bucket snapshots.
- No point-in-time recovery or AOF shipping.

## Context

- `internal/backup` (Postgres dumps), `internal/bucketbackup`, `internal/volume`;
  drivers in `internal/dbservice/drivers` (Valkey, DragonFly embed the Redis driver).
- `docs/internals/databases.md` (backups to S3; "no backups for the non-pg engines").
