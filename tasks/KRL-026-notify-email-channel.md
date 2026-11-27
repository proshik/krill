---
id: KRL-026
title: Email as a notification channel beside Telegram
kind: feature
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/notify/**
  - internal/server/**
  - internal/web/**
  - internal/database/migrations/**
  - internal/database/queries/**
  - internal/database/gen/**
  - docs/guides/notifications.md
  - docs/internals/operations.md
  - docs/configuration.md
harness: any
reviews: {spec: 1, code: 2}
created: "2026-11-27"
---

## Goal

An organization that does not use Telegram receives the same events — failures,
successful deploys, disk and memory alerts, the daily digest — by email. This also
introduces the channel-type seam the Slack channel (KRL-027) reuses.

## Acceptance

- [ ] WHEN the previous release reads a channel of a type it does not know THEN it
      skips it without sending: the release that ships email first ships the reader
      change (send only `type = 'telegram'` through the Telegram sender), and the
      migration that widens `notification_channels.type`'s CHECK lands no earlier
      than that (expand/contract: widen reads a release before writes).
- [ ] WHEN an org admin adds an email channel with SMTP settings (host, port, TLS
      mode, user, password, from, to) THEN the password is stored with `secret.Enc`
      and never shown, logged or put in an error; the host is dialed through
      `internal/netguard` (private addresses only with `KRILL_ALLOW_PRIVATE_EGRESS`).
- [ ] WHEN an event is sent THEN the email carries the same content as the Telegram
      message in the channel's language and zone, as plain text (HTML optional), and
      a send failure is logged without credentials, as for Telegram.
- [ ] WHEN **Send test message** is pressed THEN the result is shown synchronously.
- [ ] WHEN `notification_channels_org_type_uq` (one channel per type per organization)
      stays THEN the UI says so; changing it is not part of this task.
- [ ] WHEN the docs are updated THEN `docs/guides/notifications.md` and
      `docs/internals/operations.md` describe the channel; a new `KRILL_*` variable,
      if any, goes into `docs/configuration.md`.

## Checks

- Unit: rendering of every event kind and the digest as email; the SMTP password
  absent from logs and errors (in the style of `TestMCFailureNeverCarriesTheSecret`,
  `internal/dbservice/s3_test.go:26`).
- Integration: a local SMTP test server (testcontainers) receives the messages,
  with STARTTLS and implicit TLS.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- No bounce handling, no mailing lists, no templating language for users.
- Slack is KRL-027.

## Context

- `internal/database/migrations/000015_notification_channels.up.sql`
  (`type … CHECK (type IN ('telegram'))`, unique `(org_id, type)`); later columns in
  000061.
- `internal/notify/notify.go` (`Service.notify` → `sendTo` → `s.send(token, chatID,
  text)`: Telegram-shaped today), `telegram.go`, `format.go`, `digest_render.go`.
- Migration policy: AGENTS.md "Migrations (expand/contract)".
