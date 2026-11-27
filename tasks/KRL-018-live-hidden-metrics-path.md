---
id: KRL-018
title: Live acceptance of hiding an app's metrics path on a public domain that serves the metrics port
kind: research
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - docs/guides/metrics.md
  - docs/internals/operations.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The rule that hides an app's metrics path on its public domain is proven where it
matters: an app that serves metrics on the same port as its domain. The production
rollout of 2026-09-22 returned 404 for `/metrics`, `/METRICS` and `/metrics;x`, but
that app serves metrics on a separate port, so the 404 would have come without the
rule. This was ROADMAP's live-acceptance item 13.

## Acceptance

- [ ] WHEN an app that answers `/metrics` on its HTTP port is exposed on a public
      domain with a metrics endpoint on that port THEN `/metrics`, `/METRICS`, `/metrics;x` and
      `/metrics/` return 404 from the domain, while the collector still scrapes it
      (`up` = 1 on the Metrics tab).
- [ ] WHEN the metrics endpoint is removed from the app (or moved to another port) and
      the app is redeployed THEN the path is served again from the domain (the rule
      is gone).
- [ ] WHEN the app shares its host with another app by path THEN the rule still
      applies to it and does not affect the other app.

## Checks

- A dated record in `docs/guides/metrics.md` or `docs/internals/operations.md`
  (application metrics), with the HTTP status of each probe.

## Limits / non-goals

- A failure becomes its own fix task.

## Context

- `appmetrics.HiddenPaths` (`internal/appmetrics/appmetrics.go:79`: only endpoints on
  the app's own HTTP port are hidden), passed to `traefik.AppLabels` at deploy
  (`internal/deploy/deployer.go:1201`).
- `TestHiddenMetricsPathsOnRealTraefik` (`internal/traefik/labels_integration_test.go:23`,
  pinned Traefik only); the case-insensitive `PathRegexp` covering `;` parameters
  (AGENTS.md, "Shared hosts need explicit router priorities").
- `docs/guides/metrics.md`.
