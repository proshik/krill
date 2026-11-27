---
id: KRL-016
title: Live acceptance of a full install.sh run from a published release on a fresh VM
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/install.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The one-line install a new user runs is checked from start to finish against a real
GitHub release, not a local build: Docker and Swarm set up, `krill-postgres` on the
`krill-state` network, the checksum-verified binary, the generated
`/etc/krill/krill.env` and a working UI. So far only the download of the v0.1.0
binary the way the installer does it was verified against `checksums.txt`. This was
ROADMAP's live-acceptance item 7.

## Acceptance

- [ ] WHEN `install.sh` from `master` runs as root on a fresh Ubuntu LTS VM THEN it
      installs the latest release, the unit is active, the admin can sign in with the
      generated password and deploy an image app.
- [ ] WHEN the same installer runs again THEN it is an upgrade: `krill.env` and its
      secrets are kept, `krill.prev` is removed, the data survives.
- [ ] WHEN the release's `checksums.txt` entry does not match (simulated with a local
      mirror) THEN the installer refuses; with `KRILL_SKIP_VERIFY=1` it proceeds.
- [ ] WHEN the VM is rebooted THEN Krill comes back without intervention, even if
      Postgres starts slower than the unit.
- [ ] WHEN `KRILL_DATABASE_URL` points at an external Postgres THEN no local
      `krill-postgres` is created and the DSN password is never printed.

## Checks

- A dated record in `docs/install.md` or the change record: release, OS image, VM
  size, timings.

## Limits / non-goals

- A disposable VM; no production install.
- A failure becomes its own fix task.

## Context

- `install.sh` (verification at lines ~335–345); `docs/install.md`;
  `docs/internals/operations.md` (release, image and installer).
- A local VM smoke exists: `make acceptance-vm-smoke` (see the Makefile).
