---
id: KRL-022
title: Live acceptance of Krill state backups against an external verify-full database and the pre-update snapshot on a VPS
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/guides/krill-backups.md
  - docs/install.md
  - docs/internals/operations.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The two state-backup paths the VM acceptance could not cover are run for real: a
dump of an external managed Postgres reached with `sslmode=verify-full`, and the
snapshot taken before an update from Settings → Updates on a real VPS. The rest was
accepted on 2026-11-21 on a disposable Lima VM with throwaway releases
(`make acceptance-statebackup`: the bundled database backed up to S3 and restored,
runbook A after a full wipe, runbook B from the pre-update snapshot of an update
whose migration failed); that run also fixed six places in the runbooks. This was
ROADMAP's live-acceptance item 18.

## Acceptance

- [ ] WHEN `KRILL_DATABASE_URL` points at an external Postgres with `sslmode=verify-full`
      THEN a scheduled and a manual backup both succeed, the dump decrypts with the
      operator's age key, and restores into a throwaway Postgres.
- [ ] WHEN an update is started from Settings → Updates on a real VPS THEN a snapshot
      is uploaded before the binary is swapped, and the Updates page shows the
      `backing_up` phase.
- [ ] WHEN the snapshot cannot be taken (storage unreachable) THEN the update is
      refused with the reason, and pause → update → resume works as the escape hatch.

## Checks

- A dated record in `docs/guides/krill-backups.md` or `docs/internals/operations.md`
  (Krill state backup); runbook corrections, if any, in `docs/install.md`.

## Limits / non-goals

- No secret, key or DSN in the record.
- A failure becomes its own fix task.

## Context

- `internal/statebackup`, `selfupdate.Updater.install()` (pre-update hook,
  `PhaseBackingUp`), `docs/internals/operations.md` (Krill state backup).
- `docs/development.md#state-backup-acceptance`; runbooks in `docs/install.md`.
