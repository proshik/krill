---
id: KRL-015
title: Live acceptance of krill-cli deploying a locally built image through a real registry
kind: research
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - docs/guides/krill-cli.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

`krill-cli` is run end to end for the case it exists for: build an image on the
developer's machine, push it to a real registry and deploy it to a real Krill. On
2026-09-23 everything except `deploy` with a local build (and `context rm`) passed
against a real install whose app is built by CI. This was ROADMAP's
live-acceptance item 6.

## Acceptance

- [ ] WHEN `krill-cli deploy` runs in a project with `delivery: registry` against an
      image app THEN it builds, pushes only changed layers on the second run, deploys
      the derived tag and follows the deploy to its outcome; the duration of the first
      and second run is recorded.
- [ ] WHEN the repository does not match the app's image repository THEN the CLI
      refuses before building anything.
- [ ] WHEN the build fails THEN nothing is pushed or deployed.
- [ ] WHEN `krill-cli context rm` removes a context THEN its token is gone from the
      local store and other contexts still work.

## Checks

- A dated record in `docs/guides/krill-cli.md` or the change record: CLI and server
  release, registry kind (neutral name), timings.

## Limits / non-goals

- `delivery: upload` is not implemented (KRL-025) and is not tested here.
- A failure becomes its own fix task.

## Context

- `cmd/krill-cli`, `internal/krillcli` (`cli/deploy.go`, `deployflow`).
- `docs/guides/krill-cli.md`.
