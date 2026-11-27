---
id: KRL-003
title: Krill's own Telegram notifications through the link, and a relay alert when a client drops
kind: feature
status: backlog
priority: 2
component: krill
depends_on: [KRL-002]
touches:
  - internal/notify/**
  - internal/link/**
  - internal/web/i18n/**
  - cmd/krill/**
  - docs/internals/operations.md
  - docs/guides/notifications.md
  - docs/guides/link.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

A client install that cannot reach Telegram sends its own alerts and digest
through the link instead of a separate SSH SOCKS tunnel (`KRILL_TELEGRAM_PROXY`).
A link outage itself cannot be reported through the link, so the relay — which
reaches Telegram directly — reports it.

## Acceptance

- [ ] WHEN the role is `client`, the session is up and `api.telegram.org:443` is on
      the relay's destination list, and `KRILL_TELEGRAM_PROXY` is not set THEN the
      Telegram sender's transport dials through an in-process `egress` stream
      (service `krill`); WHEN `KRILL_TELEGRAM_PROXY` is set THEN it wins, as today;
      WHEN neither applies THEN messages go direct, as today.
- [ ] WHEN a relay peer has had no session for more than 2 minutes THEN the relay
      sends an alert to channels with `NotifyInfra` (operator only); on reconnect it
      sends a recovery message. `tg.link.*` texts keep sentence case. The client shows
      the outage on its Link page.
- [ ] WHEN the notifications page is shown THEN it says which path messages take
      (direct / proxy / link), the address only to an instance operator.
- [ ] WHEN docs are updated THEN `docs/internals/operations.md` (Telegram proxy
      section) and `docs/guides/notifications.md` describe the order: proxy, link,
      direct.

## Checks

- Unit with fake senders and a fake link dialer: path selection for each
  combination; outage timer (2 min threshold, one alert, one recovery); no message
  text or token in logs.
- `make test`.

## Limits / non-goals

- No digest lines about the link and no client-side "was unavailable" message in this
  version (cut in review: the relay's alert and the Link page cover the need).
- No other Krill outbound traffic (self-update, registries, observability) goes
  through the link.
- `KRILL_TELEGRAM_PROXY` is not deprecated here.

## Context

- `internal/notify/proxy.go` (`SetTelegramProxy`, `telegramClient`),
  `internal/notify/telegram.go:17` (`httpClient`), `internal/notify/store.go`
  (`NotifyInfra` is operator-only).
