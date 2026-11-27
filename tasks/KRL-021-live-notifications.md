---
id: KRL-021
title: Live acceptance of notifications beyond failures, the daily digest and the Telegram proxy
kind: research
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - docs/guides/notifications.md
  - docs/internals/operations.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The Telegram messages added in November are seen arriving from a real install
instead of fake senders: successful deploys, disk and memory alerts, the daily
digest, and messages sent through `KRILL_TELEGRAM_PROXY` from an install that
cannot reach Telegram directly. This was ROADMAP's live-acceptance item 17.

## Acceptance

- [ ] WHEN an app deploys successfully on a channel with that event on THEN one
      message arrives with the app and a working link.
- [ ] WHEN the digest time comes in the channel's time zone THEN one digest arrives,
      and a restart of Krill around that time does not send a second one.
- [ ] WHEN an instance operator has turned on infrastructure notifications for the
      channel (`NotifyInfra`; without it the digest carries no disk data,
      `internal/notify/digest.go:354-360`) THEN the digest's disk figures match `df` on
      the control plane and, over SSH, on a worker.
- [ ] WHEN a node's disk crosses the threshold THEN an alert arrives, it repeats once a
      day while it stays over, and a recovery message arrives once usage falls more than
      three percentage points below the threshold (hysteresis,
      `internal/notify/resources.go:17-46`).
- [ ] WHEN a deploy leaves the node's memory limits overcommitted (opt-in by the
      operator) THEN the warning arrives once.
- [ ] WHEN `KRILL_TELEGRAM_PROXY` points at an SSH SOCKS tunnel on an install without
      direct access THEN every message above goes through it, and a broken tunnel is
      logged without the bot token.

## Checks

- A dated record in `docs/internals/operations.md` (notifications and the digest).
- Covered so far by unit tests with fake senders, test SOCKS5/HTTP proxies and store
  tests against a real Postgres.

## Limits / non-goals

- Messages through the Krill link are KRL-003/KRL-007, not this task.
- A failure becomes its own fix task.

## Context

- `internal/notify` (`notify.go`, `digest.go`, `resources.go`, `proxy.go`, `watcher.go`).
- `docs/guides/notifications.md`; `docs/configuration.md` (`KRILL_TELEGRAM_PROXY`).
