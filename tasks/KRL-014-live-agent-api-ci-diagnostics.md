---
id: KRL-014
title: Live acceptance of the agent API from a real CI job, its diagnostics and the provisioning operations
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/internals/agent-api.md
  - docs/guides/agent-api.md
  - skills/krill-deploy/**
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The parts of the agent API added or never run since its MCP half passed on
2026-09-23 (v0.3.2, all twelve tools of the time, a real two-node cluster) are
exercised against a live install: a real CI job deploying through REST, the
diagnostics of 2026-11-26 and the provisioning operations of 2026-11-21. This was
ROADMAP's live-acceptance item 5; the provisioning operations are added from the
current code, where they exist and have no live record.

## Acceptance

- [ ] WHEN a CI job with a write token calls `POST /api/v1/apps/{ref}/deploy` with a
      new tag and polls `GET /api/v1/deployments/{id}` THEN the deploy ships that tag
      and the job sees the final status; a refusal comes back as 409 or 503 with
      `Retry-After`, never as a silent success.
- [ ] WHEN `krill_app_status` is asked about an app with a task on a worker node THEN
      it reports that task's state, health and usage.
- [ ] WHEN `krill_app_logs` is called with a time window and with `wait_for` THEN it
      returns that window, and the wait ends on the expected line (or its timeout).
- [ ] WHEN `krill_deployment_status` is called with `wait` on a deploy that goes into
      the background settle THEN it returns the settled outcome.
- [ ] WHEN `krill_domain_status` is asked about a domain with real DNS and a Let's
      Encrypt certificate THEN DNS and certificate state match what a browser sees;
      `krill_backup_now` starts a backup that finishes.
- [ ] WHEN a provision-level token creates a database, an app, a domain and a
      database link THEN each matches a UI-created one, and an identical repeat
      returns the existing resource with `created: false`.
- [ ] WHEN the results differ from `skills/krill-deploy/SKILL.md` THEN the runbooks are
      corrected.

## Checks

- A dated record in `docs/internals/agent-api.md`; the CI job's configuration in the
  record with neutral names and no token.
- Not run: swapping the token inside one MCP session from a real client (no client
  can do it); `TestMCPSessionSeesDemotion` and `TestMCPSessionRejectsAnotherToken`
  cover the binding.

## Limits / non-goals

- A failure becomes its own fix task.

## Context

- Routes: `internal/server/server.go` (`/api/v1`, `/mcp`); operations:
  `internal/api`, `internal/mcpsrv`; tool list: `docs/guides/agent-api.md`.
- `docs/internals/agent-api.md` (operations, provisioning, waiting deploys).
