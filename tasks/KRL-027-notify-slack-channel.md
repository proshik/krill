---
id: KRL-027
title: Slack as a notification channel through an incoming webhook
kind: feature
status: backlog
priority: 4
component: krill
depends_on: [KRL-026]
touches:
  - internal/notify/**
  - internal/server/**
  - internal/web/**
  - internal/database/migrations/**
  - internal/database/queries/**
  - internal/database/gen/**
  - docs/guides/notifications.md
  - docs/internals/operations.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

An organization that works in Slack gets Krill's events there, using the channel
seam KRL-026 introduces.

## Acceptance

- [ ] WHEN an org admin adds a Slack channel with an incoming-webhook URL THEN the URL
      (a bearer credential) is stored with `secret.Enc`, never shown in full, logged or
      put in an error, and only `https://hooks.slack.com/…` URLs are accepted.
- [ ] WHEN an event is sent THEN the message has the same content as the Telegram one
      (Slack mrkdwn, links to the app), in the channel's language and zone; a 429 is
      retried after `Retry-After` at most once, other failures are logged.
- [ ] WHEN the previous release reads a `slack` channel THEN it skips it (the reader
      tolerance of KRL-045 covers any unknown type, and KRL-026 has shipped).
- [ ] WHEN **Send test message** is pressed THEN the result is shown synchronously.
- [ ] WHEN the docs are updated THEN `docs/guides/notifications.md` explains creating
      the webhook in Slack.

## Checks

- Unit: rendering of every event kind and the digest; the URL never in logs.
- An `httptest` server standing in for Slack (200, 429 with `Retry-After`, 404).
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- No Slack app, OAuth or interactive buttons.

## Context

- KRL-026; `internal/notify`; `notification_channels` (migrations 000015, 000061).
