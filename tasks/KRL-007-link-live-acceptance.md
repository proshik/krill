---
id: KRL-007
title: Live acceptance of the Krill link on a real client install and a relay abroad
kind: research
status: backlog
priority: 2
component: docs
depends_on: [KRL-003, KRL-004, KRL-005]
touches:
  - ROADMAP.md
  - docs/guides/link.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The parts of the link that no local test can prove are checked on two real
installations — a client whose network cannot reach Telegram and a relay abroad —
and the results, with numbers, are recorded in the link guide and `ROADMAP.md`.

## Acceptance

- [ ] WHEN the two installs are paired THEN the link stays up for 72 hours over the
      real international path (an assumption until then: DPI lets a TLS connection
      with a private-CA certificate through); reconnects and latency are recorded.
- [ ] WHEN a long-polling bot runs on the client with egress to `api.telegram.org`
      THEN it receives and sends messages; an LLM SDK call from another app works
      without code changes; an app without egress in a granted organization still
      goes direct.
- [ ] WHEN a host goes through the relay THEN Let's Encrypt issues its certificate on
      the client over TLS-ALPN-01 **with the Telegram subnet allowlist already on**
      (and a forced renewal also passes), Telegram delivers webhooks, and other
      sources are refused on the relay path.
- [ ] WHEN the link is cut THEN egress is refused (no direct path), the relay alerts
      the operator, the link comes back by itself; a self-update on either side
      brings the link back.
- [ ] WHEN an app's selection changes THEN the egress gap of the sidecar task
      recreation is measured, and an app on a worker node is identified correctly.
- [ ] WHEN Krill sends notifications on the client THEN they go through the link,
      and through `KRILL_TELEGRAM_PROXY` when it is set.

## Checks

- A dated record per step in the guide: what was run, what was seen, the numbers.

## Limits / non-goals

- Real hosts are described with neutral stand-ins in anything committed.
- A failure becomes its own fix task.

## Context

- KRL-001…KRL-005.
