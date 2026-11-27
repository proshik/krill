---
id: KRL-010
title: Live acceptance of the persisted firewall with WireGuard, the drift view and the overlay MTU
kind: research
status: backlog
priority: 2
component: docs
depends_on: []
touches:
  - docs/internals/networking.md
  - docs/architecture.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-27"
needs_person: true
---

## Goal

The network half of the v0.7.0 platform work (ROADMAP's live-acceptance item 15) is
checked on a real cluster whose nodes talk over WireGuard: a confirmed lockdown that
survives a reboot, the per-node drift view, and `KRILL_OVERLAY_MTU` on a network
created after it is set. Until now this was proven by the firewall confirmation
protocol on real systemd 252 and 257 in a privileged container and by
`TestOverlayNetworkMTU` on a local Swarm.

## Acceptance

- [ ] WHEN a lockdown is applied and confirmed on every node THEN the ruleset is
      persisted, and after a reboot of a worker and of the control plane the
      `krill-firewall` unit reloads it before the swarm is reachable; the Firewall
      page shows no drift.
- [ ] WHEN the nodes run WireGuard THEN its listen ports are admitted and the swarm
      ports are bound to the WireGuard interface (only with a complete address list),
      and the cluster keeps working after the lockdown.
- [ ] WHEN on one node the `inet krill` table is replaced by one without Krill's
      `krill_rs_<hash>` marker, or its persisted copy is removed THEN the Firewall page
      shows that node's drift with the right reason. (An edit that keeps the marker is
      not detected: the probe compares markers, `internal/firewall/probe.go:64-74`,
      `internal/server/firewall_state.go:143-151`; record it, do not count it as a
      failure.)
- [ ] WHEN `KRILL_OVERLAY_MTU` is set to the recommended value and a new organization
      is created THEN a container on its network reports that MTU, and a large
      response through Traefik over the WireGuard path no longer stalls (measured
      before and after).
- [ ] WHEN **Open cluster** is used THEN the persisted copy is removed and a reboot
      leaves the node open.

## Checks

- A dated record in `docs/internals/networking.md` (firewall, overlay MTU): release,
  topology with neutral names, kernel and systemd versions, the measured response
  times.

## Limits / non-goals

- Recreating existing networks with a new MTU is not part of this (KRL-030).
- A failure becomes its own fix task.

## Context

- `internal/firewall` (`Apply`, `Confirm`, `armScript`, `swarmAddrSet`, probe of
  WireGuard interfaces in `probe.go`); `TestConfirmProtocolOnRealSystemd`
  (`-tags=realsystemd`).
- `docs/internals/networking.md` (the firewall dead-man switch, overlay MTU);
  `docs/architecture.md#overlay-mtu`.
- `docs/tasks/penpot-platform-follow-up.md` ("Verification log": reboot and systemd
  ordering, and the large-response reproduction, marked unverified).
