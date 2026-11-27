---
id: KRL-045
title: Notification readers skip channels of a type they do not know
kind: fix
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/notify/**
  - internal/database/queries/**
  - internal/database/gen/**
  - docs/internals/operations.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

The first step of adding channel types (email KRL-026, Slack KRL-027) under the
expand/contract rule: a release whose readers deliver only `telegram` channels
through the Telegram sender and ignore every other type. It must be released before
any release can write a channel of another type, so a rollback from that later
release lands on a binary that leaves the new rows alone. Today (v0.16.2) every
enabled channel goes through the Telegram send path, whatever its type; the CHECK
on `notification_channels.type` allows only `telegram`, so no such row exists yet.

## Acceptance

- [ ] WHEN events, infrastructure alerts and the daily digest select channels THEN
      only `type = 'telegram'` rows reach the Telegram sender (in SQL or in
      `internal/notify`, one place each path goes through), and other types are
      skipped without an error log per event.
- [ ] WHEN a channel row of an unknown type exists THEN Telegram delivery to the
      organization's Telegram channel continues for ordinary events, infra alerts and
      the digest.
- [ ] WHEN the settings page lists channels THEN an unknown type does not break the
      page (it is not shown, or shown as unsupported).

## Checks

- Store test against a real Postgres with a row of another type inserted after
  relaxing the CHECK inside the test only; unit tests for the three send paths.
- `make generate`, `make test`.

## Limits / non-goals

- No migration and no new channel type here.

## Context

- `internal/database/migrations/000015_notification_channels.up.sql` (`type … CHECK
  (type IN ('telegram'))`); `internal/notify/notify.go` (`Service.notify` → `sendTo` →
  `s.send(token, chatID, text)`), `digest.go`, `resources.go`.
- Split out of KRL-026 after the revust review of KRL-008 (r1, 2026-11-27).
