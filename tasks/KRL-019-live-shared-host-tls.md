---
id: KRL-019
title: Live acceptance of a shared host with a frontend catch-all and an API at /api on one real domain
kind: research
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - docs/guides/domains.md
  - docs/internals/networking.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

Two apps of one environment share a real public domain — a frontend as the
catch-all and an API at `/api` — with a Let's Encrypt certificate, and HTTPS is
switched for the whole host at once. Locally this runs on sslip.io over plain HTTP
only. This was ROADMAP's live-acceptance item 14.

## Acceptance

- [ ] WHEN the frontend owns the host and the API is added at `/api` THEN
      `https://<host>/api` and `https://<host>/api/x` reach the API, `/apix` and `/`
      reach the frontend, and one certificate is issued for the host.
- [ ] WHEN HTTPS is turned off and on again for the host THEN every app sharing it
      follows, plain HTTP redirects to HTTPS while it is on, and no router is left on
      the old setting.
- [ ] WHEN the API app is removed from the host THEN the frontend keeps serving `/api`
      paths as the catch-all and the certificate keeps working.

## Checks

- A dated record in `docs/internals/networking.md` (shared hosts by path), with the
  probe results.

## Limits / non-goals

- A failure becomes its own fix task.

## Context

- `internal/hostshare` (per-host advisory lock), `internal/traefik/labels.go`
  (priorities: path 200, catch-all 100, panel 1,000,000; `/api` as `Path(/api)` or
  `PathPrefix(/api/)`).
- `docs/guides/domains.md`, `docs/internals/networking.md#shared-hosts-by-path`.
