# Krill roadmap

What is done, and an index of the work that is still open.
[README.md](README.md) gives an overview of the features, and [docs/](docs/)
describes them in detail.

Open work — features, fixes, decisions and live acceptance — is tracked as tasks in
[`tasks/`](tasks/), one file per task in the [taskmoor](docs/development.md#tasks) format,
each with its goal, acceptance, checks and limits. `taskmoor ready` lists the tasks that can
be taken now, `taskmoor list` all of them with their status. This file keeps the summary of
what is done and points to the task for each open topic; the task is the source for the
details, including what already passed live.

## Done

- **Apps:** deploy from an image or a Dockerfile (Git clone + local build), deployment
  history, zero-downtime rolling updates, advanced container settings (limits,
  replicas, restart policy, healthcheck, command override) with instance-wide
  default CPU/memory limits, lifecycle controls (deploy / reload / rebuild / stop),
  volumes with S3 backup and restore, raw TCP/UDP ports, build args and secrets,
  private Git and private registries (each credential bound to its own host), a
  per-organization cap on in-flight deploys, and a periodic BuildKit cache prune.
  A deploy ships the image tag it was queued with, whatever else writes the app in
  the meantime; Reload of a stopped app redeploys it rather than restarting its old
  spec; deployment history tells UI, webhook, agent-API and control-plane deploys apart.
  Settings saved after the running deploy are marked (app header, tabs, canvas, the
  current deploy), and **apply settings** deploys them on the image that runs, without
  pulling its tag or building.
- **Projects and tenancy:** organization → project → environment → app, owner /
  admin / member roles, an instance-admin flag for cluster-wide resources, a
  private overlay network per organization (existing installs are moved onto it in
  the background, all organizations submitted before any is waited on), and
  self-service password changes
  (forced for anyone newly added to an organization).
- **Managed databases:** org-level Postgres, Redis, Valkey, DragonFly and S3 (Silo)
  instances; logical Postgres databases per environment; connection values injected
  into app env vars; external ports; Postgres backups to S3 with schedule presets,
  retention and restore. A database server with an engine the running release does
  not know is refused with an error instead of crashing Krill.
- **Buckets (v0.7.0):** env-scoped buckets on an S3 server, each with its own policy and
  access key; bucket links inject endpoint, bucket, key and region, never the root
  credentials; import of an existing bucket and key, key rotation with an explicit
  revoke, and scheduled bucket snapshots with a SHA-256 manifest, count-based retention
  and restore into a new empty bucket. See [Buckets](docs/guides/buckets.md).
- **Systems (v0.7.0):** an environment's apps grouped into systems, with the databases,
  buckets and servers each component uses. See [Systems](docs/guides/systems.md).
- **Routing:** multiple domains per app with per-domain Let's Encrypt, internal by
  default, per-domain public-path allow-lists, basic auth and IP allow-lists.
- **Shared hosts:** apps of one environment can share a domain on disjoint segment paths,
  with one catch-all, explicit router priorities and host-wide HTTPS. Domain claims,
  primary app creation and panel reservation use the same per-host database lock.
- **Domain redirect to the primary domain:** a non-primary domain can answer every
  request with a permanent redirect (301 for GET, 308 otherwise) to the app's current
  primary domain, keeping path and query, with its own Let's Encrypt certificate; the
  primary domain can be changed from the Domains tab (**Make primary**). See
  [Domains](docs/guides/domains.md#redirect-a-domain-to-the-primary-domain).
- **Operations:** live structured logs, web terminal, CPU/memory monitoring across
  all nodes, Telegram alerts, a visual cluster topology page. Telegram channels also
  report successful deploys, nodes' disks over a threshold and memory limits overcommitted
  after a deploy (opt-in by an instance operator), and send a daily digest of deploys,
  backups (with sizes), node resources and app memory/crashes, each marked 🟢/🟡/🔴, in the channel's language
  and time zone, optionally through a Telegram-only proxy (`KRILL_TELEGRAM_PROXY`) — see
  [Notifications](docs/guides/notifications.md). Since v0.7.0 a refused
  deploy says why (UI, REST, MCP), the Advanced tab shows effective limits and recent
  tasks with out-of-memory and slow-start hints, a logical database's page shows its
  connection budget and advisory locks, and the sidebar becomes a menu on narrow screens.
- **Multi-node:** worker nodes joined over SSH, placement (any / pinned / global),
  database instances pinned to a node, firewall lockdown of every node, moving a
  database instance's volume between nodes. Since v0.7.0 a confirmed lockdown is
  persisted and reloaded at boot, WireGuard listen ports are allowed and swarm ports
  bound to the WireGuard interface, the Firewall page shows per-node drift, and
  `KRILL_OVERLAY_MTU` sets the container MTU of overlay networks Krill creates (the
  Network page shows interface MTUs and probe results).
- **Automation:** GitHub push webhook and CI deploy hook, an agent API (REST +
  MCP) with org-scoped tokens, and `krill-cli` to build locally and deploy.
- **Install:** one-line `install.sh` (host binary + systemd), optionally against an
  external managed Postgres, with the control-plane's own Postgres on its own
  Docker network rather than the default bridge. The Krill UI gets its own domain and
  HTTPS from its settings page, with a confirmation through the domain and an optional
  firewall close of the direct UI port. Self-update from the UI (Settings → Updates)
  installs a newer release with a dead-man rollback to the previous binary, offered to
  release builds on `install.sh` installs.
- **Observability, stage 1 (node agent):** an opt-in Grafana Alloy agent, one global Swarm
  service task per node, ships host metrics (Prometheus remote write) and every container's
  logs (Loki) to storage you provide; Krill keeps none of it. Configured from
  **Settings → Observability**, with a check of the saved addresses that never blocks turning
  the agent on. See [Observability](docs/guides/observability.md).
- **Backups of Krill's own state:** the control-plane database is dumped on a schedule and
  before every UI update, encrypted with your age public key and uploaded to S3 storage, with a
  restore runbook. Configured from **Settings → Krill backups**. See
  [Krill backups](docs/guides/krill-backups.md).
- **Landing page:** a public site for Krill (Hugo + Hextra in `site/`, deployed to GitHub
  Pages by `pages.yml`) with what it is, a walkthrough, a quick start and links to the docs.

## Live-acceptance queue

Verified in code and tests, but not yet exercised on a real environment. Each open entry is a
task that needs a person (`needs_person: true`); entries 9 and 10 are closed and say why. The
numbers are those of earlier versions of this file, which other documents cite.

1. MinIO (now Silo) and DragonFly instances on a VPS, including S3 app-linking — [KRL-009](tasks/KRL-009-live-s3-buckets-redis-engines.md).
2. Node-down reconciliation and the database single-writer guard on two nodes — [KRL-011](tasks/KRL-011-live-node-down-single-writer.md).
3. Cluster-wide monitoring on two nodes — [KRL-013](tasks/KRL-013-live-cluster-monitoring-agent-new-node.md).
4. Moving a database instance's volume between nodes, including a failure mid-transfer — [KRL-012](tasks/KRL-012-live-db-volume-move.md).
5. The agent API with a real CI job, the 2026-11-26 diagnostics and the provisioning
   operations (the MCP half passed on 2026-09-23) — [KRL-014](tasks/KRL-014-live-agent-api-ci-diagnostics.md).
6. `krill-cli` deploying a locally built image through a real registry — [KRL-015](tasks/KRL-015-live-krill-cli-local-build.md).
7. A full `install.sh` run from a published release on a fresh VM — [KRL-016](tasks/KRL-016-live-install-from-release.md).
8. The per-organization network migration on a real cluster with services to move —
   [KRL-044](tasks/KRL-044-live-org-network-migration.md).
9. The installer's move of the control-plane Postgres onto `krill-state` on a real upgrade —
   dropped: every installer from v0.1.0, the first release, creates `krill-postgres` on
   `krill-state`, so only a pre-release install takes the move.
10. The panel domain on a real VPS — accepted on v0.3.2 (2026-09-22/23) after the two fixes the
    first attempt found; a recurrence of the unexplained swarm-check report is now logged with
    its reason.
11. Self-update: **Roll back** on a real server, the starting page through the panel domain and
    a reboot inside the rollback window — [KRL-017](tasks/KRL-017-live-self-update-rollback.md).
12. The observability agent on a node added later — [KRL-013](tasks/KRL-013-live-cluster-monitoring-agent-new-node.md).
13. Hiding an application's metrics path on a public domain that serves the metrics port —
    [KRL-018](tasks/KRL-018-live-hidden-metrics-path.md).
14. A shared host: a frontend catch-all and an API at `/api` on one real domain — [KRL-019](tasks/KRL-019-live-shared-host-tls.md).
15. The v0.7.0 platform work on a real cluster: buckets, Valkey and systems — [KRL-009](tasks/KRL-009-live-s3-buckets-redis-engines.md); the
    persisted firewall with WireGuard, the drift view and the overlay MTU — [KRL-010](tasks/KRL-010-live-firewall-wireguard-mtu.md).
16. Domain redirect to the primary domain: certificate renewal and the redirect after
    **Make primary** — [KRL-020](tasks/KRL-020-live-redirect-renewal-make-primary.md).
17. Notifications beyond failures, the daily digest and `KRILL_TELEGRAM_PROXY` — [KRL-021](tasks/KRL-021-live-notifications.md).
18. Krill state backups with an external `verify-full` database and the pre-update snapshot on
    a VPS — [KRL-022](tasks/KRL-022-live-state-backup-vps.md).

The Krill link has its own live acceptance, [KRL-007](tasks/KRL-007-link-live-acceptance.md).

## Open

- **Krill link** — a paired relay install abroad for egress, ingress and notifications:
  [KRL-001](tasks/KRL-001-link-core.md) (core), [KRL-002](tasks/KRL-002-link-egress.md) (egress), [KRL-003](tasks/KRL-003-link-notifications.md) (notifications), [KRL-004](tasks/KRL-004-link-api-operator-token.md) (agent API), [KRL-005](tasks/KRL-005-link-ingress.md)
  (ingress), [KRL-006](tasks/KRL-006-link-topology.md) (topology).
- **Resilience on top of Swarm** — a resilient ingress and a decision on high availability vs.
  a fast-recovery runbook for the control plane: [KRL-023](tasks/KRL-023-resilience-decision.md).
- **Re-provision logical databases on a DB-instance deploy**, so a restore onto a new host
  needs no manual SQL: [KRL-024](tasks/KRL-024-reprovision-logical-dbs-on-deploy.md).
- **Direct image upload** in `krill-cli` (`delivery: upload`): [KRL-025](tasks/KRL-025-cli-direct-image-upload.md).
- **Email and Slack** notification channels: [KRL-045](tasks/KRL-045-notify-skip-unknown-channel-types.md)
  (readers that skip unknown channel types, released first), [KRL-026](tasks/KRL-026-notify-email-channel.md), [KRL-027](tasks/KRL-027-notify-slack-channel.md).
- A **Homebrew tap** for `krill-cli`: [KRL-028](tasks/KRL-028-homebrew-tap-krill-cli.md).
- **Existing MinIO instances** onto Silo in place: [KRL-029](tasks/KRL-029-minio-to-silo-in-place.md).
- **Existing overlay networks keep their MTU** — automate their recreation or keep the
  [documented procedure](docs/architecture.md#overlay-mtu): [KRL-030](tasks/KRL-030-existing-overlay-mtu-decision.md).
- A **cap on organizations per user**: [KRL-031](tasks/KRL-031-org-cap-per-owner.md).
- **Observability, stage 3 (traces)** over OTLP through the node agent: [KRL-032](tasks/KRL-032-observability-traces.md).
- **Move the Docker client off `github.com/docker/docker` v28** onto the moby client and api
  modules: [KRL-033](tasks/KRL-033-moby-client-modules.md).

## Later

- Federation across data centers with GeoDNS, as disaster recovery, strictly after the
  resilience work: [KRL-034](tasks/KRL-034-federation-geodns-design.md).
- Backups for Valkey and DragonFly: [KRL-035](tasks/KRL-035-backups-valkey-dragonfly.md); a domain for the S3 console: [KRL-036](tasks/KRL-036-s3-console-domain.md);
  OpenSearch as a managed engine with env-scoped indices: [KRL-037](tasks/KRL-037-opensearch-engine-design.md).
- Encrypted node-to-node traffic: [KRL-038](tasks/KRL-038-node-traffic-encryption-design.md); database replication: [KRL-039](tasks/KRL-039-database-replication-design.md).
- A GitHub App or OAuth instead of webhook + token: [KRL-040](tasks/KRL-040-github-app-design.md); SSH deploy keys: [KRL-041](tasks/KRL-041-ssh-deploy-keys.md).
- **Signed releases:** a signed `checksums.txt`, verified by the installer and the
  self-updater: [KRL-042](tasks/KRL-042-signed-releases.md).
- **Installer-required marker:** a release asset that lets the Updates page refuse a release
  that needs `install.sh`: [KRL-043](tasks/KRL-043-installer-required-marker.md).

## Not planned for now

Docker Compose, MySQL / MariaDB / MongoDB, buildpacks, preview deployments, custom
RBAC, 2FA and SSO.

## Application metrics (observability stage 2)

Implemented in code: per-app endpoints and encrypted bearer tokens, deployment-time injection, protection of metrics paths on public domains, an organization-network Alloy collector, and live scrape status on the Metrics tab. See [Application metrics](docs/guides/metrics.md).

Disposable two-node live acceptance passed with four application replicas, per-node labels, token rotation and recovery, public metrics-path protection, network cooldown, and firewall isolation. Rolled out to a production install on 2026-09-22: one application collected (`up`), the token enforced (401 without it), the collector's own memory measured by `anon`. Hiding the metrics path on a domain that serves the metrics port is still open (live-acceptance queue, item 13: [KRL-018](tasks/KRL-018-live-hidden-metrics-path.md)).
