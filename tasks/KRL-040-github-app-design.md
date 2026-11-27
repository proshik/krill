---
id: KRL-040
title: Decide whether a GitHub App (or OAuth) replaces the per-app webhook secret and personal access token
kind: design
status: backlog
priority: 4
component: docs
depends_on: []
touches:
  - docs/internals/deploy.md
harness: any
reviews: {spec: 1, code: 0}
created: "2026-11-27"
---

## Goal

Auto-deploy from GitHub today needs, per app, a webhook the user adds by hand
(verified by a per-app secret at `/webhooks/github/{appID}`) and, for private
repositories, a personal access token stored for the clone. A GitHub App would
install once per account, give short-lived installation tokens scoped to chosen
repositories and deliver webhooks itself. This task decides whether that is worth
it for a self-hosted panel, where each install would register its own App.

## Acceptance

- [ ] WHEN the design is recorded THEN `docs/internals/deploy.md` describes the App
      registration flow for a self-hosted install (manifest flow, where the private key
      is stored, how the callback reaches an install behind the panel domain), the
      token lifetime and refresh, how existing webhook + token apps keep working, and
      the cost — or why the current model stays.
- [ ] WHEN the decision is to build it THEN feature tasks are written.

## Checks

- One spec review round. No code.

## Limits / non-goals

- SSH deploy keys are a separate, smaller task (KRL-041).

## Context

- `internal/server/server.go` (`/webhooks/github/{appID}`, `/webhooks/deploy/{appID}`),
  `internal/server/webhook_handlers.go`, `internal/builder/git.go` (token over
  `GIT_ASKPASS`); `docs/internals/deploy.md` (GitHub auto-deploy, credential host
  binding).
