---
id: KRL-017
title: Live acceptance of self-update rollback, the starting page through the panel domain and a reboot in the rollback window
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/internals/operations.md
  - docs/install.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The three parts of self-update that were never run on a real server are checked
against real `proshik/krill` releases. Everything else passed: on 2026-09-15 on a
disposable Lima VM against throwaway releases (`make acceptance-selfupdate`), and on
a real two-node cluster on 2026-09-22 (v0.3.0 → v0.3.1, schema 48 → 49) and
2026-09-23 (v0.3.1 → v0.3.2). This was ROADMAP's live-acceptance item 11.

## Acceptance

- [ ] WHEN **Roll back** is pressed on a real server after an update THEN the previous
      binary (`krill.prev`) runs, the page reports the rollback, and the schema the
      newer release applied does not stop it from starting.
- [ ] WHEN the UI is opened through the panel domain during an update's restart THEN
      the browser sees the "Krill is starting" page or a short Traefik 502/504 that
      the Updates page rides out, and the page reports the result without a manual
      reload; the length of the gap is recorded.
- [ ] WHEN the server is rebooted inside the 10-minute rollback window THEN the
      outcome is the one `docs/install.md` describes (the `/run` markers and
      transient timers are gone after the reboot), and the install is left in a
      working state. Immediately before the reboot, the record shows the new binary in
      place, startup not yet confirmed (`/run/krill-update-pending` present) and the
      `krill-update-revert` timer armed — otherwise `ConfirmStartup` already ran and the
      reboot tests nothing (an unconfirmed state can be held by a new binary that does
      not reach serving, e.g. its database made unreachable). After the boot: the
      running version, `krill.prev` kept, no timer left.

## Checks

- A dated record in `docs/internals/operations.md` (self-update) and, where an
  operator step changes, in `docs/install.md`.

## Limits / non-goals

- Uses releases that already exist; no release is cut for this task.
- A failure becomes its own fix task.

## Context

- `internal/selfupdate` (`Updater`, markers in `markers.go`, revert in `revert.go`),
  `server.StartupGate`; `docs/internals/operations.md` (self-update, the pending
  marker ordering, never restarting from inside the unit's cgroup).
- `docs/development.md#self-update-acceptance`.
