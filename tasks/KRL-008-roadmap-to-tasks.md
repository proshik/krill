---
id: KRL-008
title: Move the open items of ROADMAP.md into taskmoor tasks
kind: chore
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - ROADMAP.md
  - AGENTS.md
  - CONTRIBUTING.md
  - docs/development.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

Open work in Krill is tracked as taskmoor tasks in `tasks/`, the way the Krill
link (KRL-001…KRL-007) already is, instead of prose in `ROADMAP.md` and the
maintainer's local plans. A contributor or an agent finds takeable work with
`taskmoor ready`.

## Acceptance

- [ ] WHEN `ROADMAP.md` is read THEN every open item (including the live-acceptance
      queue) is either a task `KRL-<n>` with Goal / Acceptance / Checks / Limits /
      Context written from the current code, or listed in the change's record as
      dropped or already done, with the reason.
- [ ] WHEN the tasks are written THEN they are self-contained: the maintainer's
      local-only `docs/superpowers/` files may be cited as context, never relied on.
- [ ] WHEN the change lands THEN `ROADMAP.md` keeps the done/status summary and
      points to `tasks/` for open work, and AGENTS.md (Dev workflow) and
      `docs/development.md` say how tasks are written and merged (`Task: <ID>`
      trailer).
- [ ] WHEN the change is checked THEN `taskmoor validate` reports no errors.

## Checks

- `taskmoor validate`; a side-by-side list of ROADMAP items and their tasks in the
  change's record.

## Limits / non-goals

- No code changes. The maintainer's existing local plans are not converted; only
  `ROADMAP.md` is the source.

## Context

- `ROADMAP.md`, `tasks/tasks.toml`, the taskmoor format (`spec/README.md` of the
  taskmoor repository).
