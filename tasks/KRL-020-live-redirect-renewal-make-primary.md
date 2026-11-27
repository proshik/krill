---
id: KRL-020
title: Live acceptance of certificate renewal on a redirecting domain and the redirect after Make primary
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

The two parts of the redirect to the primary domain that a first issuance cannot
show are checked live: a redirecting domain renewing its certificate over HTTP-01
through the redirect host, and the old primary redirecting after **Make primary**.
On 2026-11-16 a real three-node cluster self-updated from v0.7.0 to v0.8.0 and two
TLS redirecting domains got their certificates on the first try; both answer 301 to
GET and 308 to POST with the path and query kept, and plain HTTP reaches the target
in one hop. This was ROADMAP's live-acceptance item 16.

## Acceptance

- [ ] WHEN a redirecting domain's certificate reaches Traefik's renewal window (about
      two months after the 2026-11-16 issuance) THEN it is renewed without a manual
      step and the redirect keeps answering over HTTPS.
- [ ] WHEN **Make primary** moves the primary to another domain of a live app THEN the
      former primary, if set to redirect, sends 301/308 to the new primary with path
      and query kept, and the new primary serves the app.
- [ ] WHEN the redirect is switched off on a domain THEN it serves the app again.

## Checks

- A dated record in `docs/internals/networking.md` (redirects to the primary domain):
  certificate dates before and after renewal, the probe results.

## Limits / non-goals

- Renewal cannot be hurried on the real certificate; the box waits for the date.
- A failure becomes its own fix task.

## Context

- `domains.redirect_to_primary`; `internal/traefik/labels.go`;
  `docs/guides/domains.md#redirect-a-domain-to-the-primary-domain`.
