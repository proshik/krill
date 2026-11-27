---
id: KRL-008
title: Move the open items of ROADMAP.md into taskmoor tasks
kind: chore
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - ROADMAP.md
  - AGENTS.md
  - CONTRIBUTING.md
  - docs/development.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
---

## Goal

Open work in Krill is tracked as taskmoor tasks in `tasks/`, the way the Krill
link (KRL-001…KRL-007) already is, instead of prose in `ROADMAP.md` and the
maintainer's local plans. A contributor or an agent finds takeable work with
`taskmoor ready`.

## Acceptance

- [x] WHEN `ROADMAP.md` is read THEN every open item (including the live-acceptance
      queue) is either a task `KRL-<n>` with Goal / Acceptance / Checks / Limits /
      Context written from the current code, or listed in the change's record as
      dropped or already done, with the reason.
- [x] WHEN the tasks are written THEN they are self-contained: the maintainer's
      local-only `docs/superpowers/` files may be cited as context, never relied on.
- [x] WHEN the change lands THEN `ROADMAP.md` keeps the done/status summary and
      points to `tasks/` for open work, and AGENTS.md (Dev workflow) and
      `docs/development.md` say how tasks are written and merged (`Task: <ID>`
      trailer).
- [x] WHEN the change is checked THEN `taskmoor validate` reports no errors.

## Checks

- `taskmoor validate`; a side-by-side list of ROADMAP items and their tasks in the
  change's record.

## Limits / non-goals

- No code changes. The maintainer's existing local plans are not converted; only
  `ROADMAP.md` is the source.

## Context

- `ROADMAP.md`, `tasks/tasks.toml`, the taskmoor format (`spec/README.md` of the
  taskmoor repository).

## Change record (2026-11-27)

Every open item of `ROADMAP.md` at `f9944da`, checked against the code at that commit.
Item 9 is dropped, item 10 and the landing page are done; everything else is a task. What already passed live is kept in each task's Goal or Context.

**Live-acceptance queue**

| # | ROADMAP item | Outcome |
|---|---|---|
| 1 | MinIO and DragonFly on a VPS, S3 app-linking | KRL-009 |
| 2 | Node-down reconciliation + single-writer fix on two nodes | KRL-011 |
| 3 | Cluster-wide monitoring on two nodes ("to confirm") | KRL-013 |
| 4 | DB volume move between nodes, failure mid-transfer | KRL-012 |
| 5 | Agent API: real CI job, the 2026-11-26 diagnostics | KRL-014 (adds the 2026-11-21 provisioning operations, which exist in `internal/server/api_provision.go` and have no live record) |
| 6 | `krill-cli` local build + `context rm` | KRL-015 |
| 7 | Full `install.sh` from a published release on a fresh VM | KRL-016 |
| 8 | Per-organization network migration on a real upgrade | KRL-044. (First dropped as "pre-v0.1.0 only"; the review showed the path is reachable on releases: `SeedAdmin` creates `Default` without a network, and at v0.1.0 a failed `SetOrganizationNetwork` left an organization without its name.) |
| 9 | Installer's Postgres move onto `krill-state` on a real upgrade | **Dropped.** `krill-state` entered `install.sh` in `18170a1`, contained in v0.1.0 (2026-09-12), the first release; every installer since creates `krill-postgres` on `krill-state`, so only a pre-release install takes the move. |
| 10 | Panel domain on a real VPS | **Done:** ROADMAP itself records it accepted on v0.3.2 (2026-09-22/23); the unexplained swarm-check report is logged with its reason since v0.3.2. |
| 11 | Self-update: Roll back, starting page via panel domain, reboot in the window | KRL-017 |
| 12 | Observability agent on a node added later | KRL-013 |
| 13 | Hidden metrics path on a same-port app | KRL-018 |
| 14 | Shared host: frontend catch-all + `/api`, Let's Encrypt, HTTPS toggle | KRL-019 |
| 15 | v0.7.0 platform work on a real cluster | KRL-009 (buckets, Valkey, systems), KRL-010 (firewall with WireGuard, drift, overlay MTU) |
| 16 | Domain redirect: renewal, redirect after Make primary | KRL-020 |
| 17 | Notifications beyond failures, digest, `KRILL_TELEGRAM_PROXY` | KRL-021 |
| 18 | State backups: external `verify-full`, pre-update snapshot on a VPS | KRL-022 |

**Open**

| ROADMAP item | Outcome |
|---|---|
| Resilience on top of Swarm (resilient ingress; HA vs. fast-recovery runbook) | KRL-023 (design; still one Traefik replica on the manager, `internal/traefik/bootstrap.go:184-196`) |
| Re-provision logical databases on DB-instance deploy | KRL-024 (`deployCore` provisions nothing, `internal/dbservice/instance.go:160`; manual SQL in runbook A of `docs/install.md`) |
| Direct image upload in `krill-cli` | KRL-025 (the CLI still refuses `delivery: upload`, `internal/krillcli/cli/root.go`) |
| Email and Slack channels | KRL-045 (readers skip unknown types, released first), KRL-026, KRL-027 (`notification_channels.type` CHECK allows only `telegram`, migration 000015) |
| Homebrew tap for `krill-cli` | KRL-028 (no tap in `.github/workflows/release.yml`) |
| Existing MinIO instances onto Silo | KRL-029 |
| Existing overlay networks keep their MTU | KRL-030 (design: the owner's choice between automation and the documented procedure is pending) |
| Per-owner cap on organizations | KRL-031 (`createOrg` has no cap) |
| Observability stage 3 (traces) | KRL-032 (no OTLP in `internal/observability`) |
| Docker client off `github.com/docker/docker` v28 | KRL-033 (still `v28.5.2+incompatible` in `go.mod`; now 29 files, 10 non-test, not 25/9 as ROADMAP said) |

**Later**

| ROADMAP item | Outcome |
|---|---|
| Federation with GeoDNS | KRL-034 (design, depends on KRL-023) |
| OpenSearch; a domain for the S3 console; backups for Valkey and DragonFly | KRL-037 (design), KRL-036, KRL-035 |
| Encrypted node-to-node traffic; database replication | KRL-038, KRL-039 (design) |
| GitHub OAuth / GitHub App; SSH deploy keys | KRL-040 (design), KRL-041 |
| Landing page | **Done:** `site/` (Hugo + Hextra) and `.github/workflows/pages.yml`, added in `a50e6b7` (2026-11-25); moved to ROADMAP's Done list. |
| Signed releases | KRL-042 (no signature in `release.yml`, `install.sh` or `internal/selfupdate`) |
| Installer-required marker | KRL-043 |

**Not planned for now** (Docker Compose, MySQL / MariaDB / MongoDB, buildpacks, preview
deployments, custom RBAC, 2FA, SSO): no tasks; the section stays in `ROADMAP.md` as the
owner's record of what is out of scope.

**Application metrics (observability stage 2)**: done; its one open point is item 13
(KRL-018).

Documents outside this task's `touches` that still describe `ROADMAP.md` as the place for
open work or the live-acceptance queue, left as they are: `README.md` (line 121),
`docs/internals/README.md` (lines 27, 32), notes in `docs/internals/*.md` that say an
acceptance "remains in ROADMAP" (the queue numbers they cite are kept), the comment in
`internal/krillcli/client/client.go:37` ("Direct image upload" is still a heading-level item
in ROADMAP's Open list), `site/layouts/_partials/custom/footer.html` and
`.github/ISSUE_TEMPLATE/feature_request.yml`. All their links still resolve.

**Review.** One revust round (`codex-only` profile unchanged: four finders, synthesis and
verify on codex `gpt-6.2-sol`, effort high; complete, 4/4 sources) on commit `85710cf`
returned 28 confirmed or refined findings, all checked against the code and applied in the
follow-up commit: item 8 restored as KRL-044 (see above); KRL-026 split, its reader
compatibility now KRL-045 with KRL-026 depending on it; acceptance made checkable or
corrected against the code in KRL-010 (drift is detected by marker only), KRL-011 (evidence
of lock contention), KRL-012 (mid-copy recovery scales the source back up), KRL-013 ("no
data", no unreachable state), KRL-014 (409 has no `Retry-After`; `wait_for` does not combine
with a window), KRL-017 (the pending state checked before the reboot), KRL-020 (the former
primary's redirect turned on explicitly), KRL-021 (`NotifyInfra`, the recovery hysteresis);
design gaps written into KRL-024 (creation and re-provision serialized, runbook and
acceptance tests in scope, a conditional manual path kept), KRL-025 (OCI index names, local
image identity for pre-deploy, control-plane constraint for "any node"), KRL-029 (a
cross-server copy for the fallback), KRL-031 (count by `organizations.owner_id`), KRL-035 (a
separate table, the restore runbook's pause list), KRL-041 (`secret.Seal`, never plaintext),
KRL-042 (publication order, hosts that skip the key bridge), KRL-043 (older updaters ignore
the marker); the KRL-033 count corrected to 10 production files; `--person` for
`needs_person` tasks added to `docs/development.md` and AGENTS.md.
