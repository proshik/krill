---
id: KRL-043
title: A release asset that marks a release as needing install.sh, honored by the Updates page
kind: feature
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - .github/workflows/release.yml
  - internal/selfupdate/**
  - internal/server/update_handlers.go
  - internal/web/**
  - docs/install.md
  - docs/internals/operations.md
  - CONTRIBUTING.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

CONTRIBUTING.md says a release that cannot avoid requiring the installer must say
"requires re-running install.sh" in its release notes. The Updates page cannot read
release notes, so it would offer such a release and the update would leave the host
half-upgraded. A machine-readable asset lets the page refuse it and point at the
installer.

## Acceptance

- [ ] WHEN a release needs the installer THEN its assets include a small file (for
      example `requirements.json` with `{"installer_required": true, "reason": "…"}`)
      that the release job adds when a marker in the repository says so, and that is
      covered by `checksums.txt`.
- [ ] WHEN the Updates page offers that release THEN it shows the reason and the
      install.sh command instead of the install button, and the install endpoint
      refuses it too (not only the page).
- [ ] WHEN the asset is missing (every release so far) THEN behavior is unchanged.
- [ ] WHEN several releases are skipped THEN a marker on any release between the
      running one and the target blocks the update (the checker reads the markers of
      the releases in between, or the marker is cumulative — the spec decides).
- [ ] WHEN the docs are updated THEN CONTRIBUTING.md's rule names the marker, and
      `docs/install.md` / `docs/internals/operations.md` describe the refusal.

## Checks

- Unit: checker with and without the asset, a skipped marked release; handler refusal.
- `make generate`, `make test`.

## Limits / non-goals

- The asset carries no executable steps.

## Context

- `internal/selfupdate/checker.go` (discovery through the `releases/latest` redirect,
  no GitHub API), `install.go`; `internal/server/update_handlers.go`.
- CONTRIBUTING.md ("Host-side upgrade steps must be idempotent startup code").
