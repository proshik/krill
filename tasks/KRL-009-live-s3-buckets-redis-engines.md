---
id: KRL-009
title: Live acceptance of managed S3 (Silo), buckets, DragonFly and Valkey on a real install
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/internals/databases.md
  - docs/guides/buckets.md
  - docs/guides/systems.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The storage engines that so far ran only in integration tests against real images
are exercised on a real install with real apps: an S3 server and its buckets, a
DragonFly and a Valkey instance, each linked into an app, and the systems view of
an environment that uses them. This was ROADMAP's live-acceptance items 1 and the
storage half of item 15 (the v0.7.0 platform work planned with an adoption of
Penpot-like workloads).

## Acceptance

- [ ] WHEN an S3 server (engine `minio`, image `drivers.MinIOImage`, Silo) is created
      on a VPS and linked to an app through an instance link THEN the app receives
      `endpoint`/`access_key`/`secret_key`/`region` and reads and writes objects; the
      S3 API and console external ports work when set and are gone when cleared.
- [ ] WHEN an existing bucket and key are imported and an app is switched from a
      literal env value to a bucket link THEN the app keeps working with the
      restricted key, and the restricted key is refused on another bucket.
- [ ] WHEN a bucket snapshot runs THEN `manifest.json` is written last and lists every
      object with its SHA-256, retention keeps the configured count of complete
      snapshots, and a restore into a new empty bucket returns the same objects.
- [ ] WHEN a key is rotated THEN the app works after a redeploy with the new key, and
      the old key keeps working until the explicit revoke and stops after it.
- [ ] WHEN a DragonFly instance and a Valkey instance are created and linked THEN an
      app connects with the injected values, data survives a redeploy of the
      instance, and the directory lock keeps a second container blocked while the
      first runs (no two writers on the volume).
- [ ] WHEN a Valkey that ran as an ordinary app is replaced by a managed Valkey THEN
      the switch-over steps (data transfer or an explicitly accepted cache discard)
      are written down with the measured outage.
- [ ] WHEN the environment is grouped into systems THEN each system shows the
      databases, buckets and servers its components actually use.

## Checks

- A dated record per step in `docs/internals/databases.md` (or the guides): what ran,
  on which release and topology (neutral host names), what was seen.
- Not run here: moving an old `quay.io/minio/minio` instance onto Silo (KRL-029).

## Limits / non-goals

- Real hosts, buckets and keys are described with neutral stand-ins; no credential
  is written anywhere.
- A failure becomes its own fix task.

## Context

- Covered so far by `TestBucketLifecycleOnSilo` and `TestBucketBackupAndRestoreOnSilo`
  (`internal/dbservice`, `-tags=integration`) and the Valkey driver test on the real
  image; DragonFly and the S3 driver: `internal/dbservice/drivers/`.
- Bucket design and invariants: `docs/internals/databases.md` (S3 buckets, bucket
  backups); user guide: `docs/guides/buckets.md`, `docs/guides/systems.md`.
- The tracked adoption notes: `docs/tasks/penpot-platform-follow-up.md`
  (section "Phased implementation and safe Penpot adoption").
