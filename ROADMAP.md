# Krill roadmap

What is done, what is waiting for live verification, and what is open.
[README.md](README.md) gives an overview of the features, and [docs/](docs/)
describes them in detail.

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

## Live-acceptance queue

Verified in code and tests, but not yet exercised on a real environment:

1. MinIO and DragonFly instances on a VPS, including S3 app-linking.
2. Node-down reconciliation — a live drain/remove on a cluster. This queue item now also
   covers the single-writer fix (stop-first updates + a portable directory lock, not
   `flock --no-fork` — busybox/alpine images don't support that flag — on Postgres/Redis/
   DragonFly instances — see [Nodes](architecture.md#nodes)), found live on 2026-09-18 when a
   worker's return produced two Postgres postmasters on the same volume (overlapping ~0.3s,
   then the new one serving for ~60s on a directory the old one had already shut down, until
   Postgres's once-a-minute lock-file recheck stopped it). Live
   acceptance on two nodes should specifically check: return a worker holding a database
   instance mid-reconciliation and confirm the second container's postmaster stays blocked
   (doesn't start) until the first is actually gone, and that its log then has NO "was not
   properly shut down" line once it does start — i.e. no crash recovery, meaning the two never
   actually overlapped. Also confirm an instance that was only Started/Stopped (never
   redeployed) since the fix shipped is still NOT protected — Start/Stop reuse the old spec —
   so it needs an explicit Deploy first.
3. Cluster-wide monitoring on two nodes (most likely already passed; to confirm).
4. Moving a database instance's volume between nodes on a real cluster, including
   a node failing mid-transfer.
5. The agent API with a real MCP client and a real CI job. The MCP half passed on 2026-09-23 on a
   real two-node cluster (v0.3.2): all twelve tools, against a live app. The per-request identity and
   session-to-token binding of v0.3.2 is covered by `TestMCPSessionSeesDemotion` and
   `TestMCPSessionRejectsAnotherToken` only; that install has a single member and no client that can
   swap the token inside a session. Still open: a real CI job calling the REST deploy hook.
6. `krill-cli` end to end against a real Krill and a real registry. Everything except `deploy` with a
   local image build (and `context rm`) passed on 2026-09-23 against a real install; that install's
   app is built by CI, so there was nothing for the CLI to build.
7. A full `install.sh` run from a published release on a fresh VM. `v0.1.0` exists, and
   downloading its binary the way the installer does was verified against `checksums.txt`,
   but the whole install from it has not been run.
8. The per-organization network migration on a real Swarm cluster — an upgrade
   of an install with existing apps and database instances, moving them all
   onto their organization's network without an outage.
9. The installer's control-plane Postgres move (`krill-state` network) on a
   real VPS upgrade, including the container-recreation path.
10. The panel domain on a real VPS: Traefik's HTTP provider reaching Krill at the
    advertise address, Let's Encrypt issuance, confirmation through the domain, and closing
    direct access under lockdown (the `docker_gwbridge` rule) with the dead-man switch.
    The first attempt (2026-09-22, two-node cluster, v0.3.1) found that a repeated close
    overwrote the dead-man snapshot and that the snapshot added its rules to the table instead
    of replacing it; both were fixed in v0.3.2. A single close + confirmation then passed on the same
    cluster, and on v0.3.2 the page shows the closed state correctly with no drift warning. Accepted.
    Why the first attempt's swarm check reported dropped nodes that stayed Ready is still
    unexplained; since v0.3.2 the check logs its reason, so a recurrence will say.
11. Self-update on a real VPS from a real `proshik/krill` release. Everything else in the flow was accepted on
    2026-09-15 on a disposable Lima VM (Ubuntu 24.04, real systemd, `install.sh`) against throwaway releases
    (`make acceptance-selfupdate`): update, manual rollback, crash-loop automatic rollback, a deploy started
    during the download aborting the update, `install.sh` over a UI update, SIGTERM during a migration, and
    the documented recovery from a failed migration. The update itself passed on a real two-node cluster on
    2026-09-22: v0.3.0 → v0.3.1 from Settings → Updates, confirmed within seconds of the restart, `krill.prev`
    kept, schema 48 → 49 applied cleanly; again on 2026-09-23, v0.3.1 → v0.3.2 (no migrations).
    Still open: **Roll back** on a real server, the starting page
    through the panel domain (Traefik 502/504 while Krill restarts) and a reboot inside the 10-minute
    rollback window.
12. The observability agent on a real two-node cluster. Per the 2026-09-23 acceptance report everything
    below passed except the agent appearing on a node added later (no new node is planned on that
    cluster): the agent appears on a node added
    later, host metrics and container logs with `krill_*` labels reach a real Prometheus/Mimir
    and Loki, memory of the agent under load, Check against Mimir with authentication, a worker
    down or drained during a settings change (global update with parallelism 1), an authenticated
    receiver (the 0400 root-owned secret is readable by Alloy), and memory over a long window (it
    was still rising at 14 minutes).
13. Hiding an application's metrics path on its public domain, on a real install with an app
    whose metrics are served on the same port as its domain. The production rollout of 2026-09-22
    returned 404 for `/metrics`, `/METRICS` and `/metrics;x`, but that app serves metrics on a
    separate port, so the 404 would have come without the rule. The case-insensitive and
    `;`-parameter matching is covered by `TestHiddenMetricsPathsOnRealTraefik` against the pinned
    Traefik only.

14. Shared host: a frontend catch-all and API at `/api` on one real domain, including
    Let's Encrypt issuance and toggling HTTPS for all apps sharing the host.
15. The v0.7.0 platform work on a real cluster, planned with the Penpot adoption
    ([follow-up plan](docs/tasks/penpot-platform-follow-up.md#phased-implementation-and-safe-penpot-adoption)):
    importing an existing bucket and key and switching an app to bucket links, a bucket
    snapshot and its restore, managed Valkey replacing a Valkey app, systems, the
    persisted firewall surviving a reboot with WireGuard and the drift view, and the
    overlay MTU on a newly created network. Covered so far by integration tests against
    the real Silo, Valkey and Traefik images and by the firewall confirmation protocol
    checked on real systemd 252 and 257.
16. Domain redirect to the primary domain on a real install: a TLS redirecting domain
    obtaining and renewing its Let's Encrypt certificate (HTTP-01 through the redirect
    host) and the redirect from the old host after **Make primary**. Checked so far
    against the pinned Traefik locally with plain HTTP and a self-signed certificate.
    On 2026-11-16 a real three-node cluster self-updated from v0.7.0 to v0.8.0, and two TLS
    redirecting domains on it got Let's Encrypt certificates on the first try. Both answer
    `301` to `GET` and `308` to `POST` with the path and query kept, and plain HTTP goes to
    the target in one hop. Still open: certificate renewal (due in about two months) and the
    redirect after **Make primary** on a live install, since those primaries were set at app
    creation.
17. Notifications beyond failures on a real install: a successful-deploy message, the daily
    digest arriving at its scheduled time in the channel's zone (and once only across a
    restart), disk snapshots on the control plane and over SSH on a worker (`df` on the real
    hosts), a disk alert, its daily repeat and its recovery, and the memory-overcommit warning
    after a deploy, and messages through `KRILL_TELEGRAM_PROXY` (an SSH SOCKS tunnel) from an
    install that cannot reach Telegram directly. Covered so far by unit tests with fake senders,
    test SOCKS5/HTTP proxies and store tests against a real Postgres. See
    [Notifications](docs/guides/notifications.md).
18. Krill state backups on a real install: an external database with `verify-full`, and the
    pre-update snapshot on a real VPS (including a blocked update and the pause escape hatch).
    Accepted on 2026-11-21 on a disposable Lima VM with throwaway releases
    (`make acceptance-statebackup`): the bundled database backed up to S3 (MinIO) and restored
    into a throwaway container, runbook A on one VM after a full wipe (a newer release
    reinstalled, the dump restored and brought forward by its migration, the old password and
    operator kept, old sessions refused, a database server, its logical database and backup,
    and an app recovered, Back up now working on the restored system), and runbook B from the
    pre-update snapshot of an update whose migration failed, with its revert timer still armed.
    The run also fixed six places in those runbooks (`docs/install.md`).

## Open

- **Resilience on top of Swarm:** a resilient ingress (Traefik is a single replica
  pinned to the manager) and a decision on high availability vs. a documented
  fast-recovery runbook for the control plane.
- **Re-provision logical databases on DB-instance deploy**, so restoring onto a new host
  needs no manual `CREATE USER`/`CREATE DATABASE` (today a step of the
  [restore runbook](docs/install.md#restore-krills-state-from-a-backup)).
- **Direct image upload** in `krill-cli` (`delivery: upload`) for users without a
  registry — designed, not implemented; the CLI refuses the mode for now.
- **Email and Slack** notification channels (Telegram ships today, with successful-deploy
  messages, disk and memory alerts and a daily digest).
- A Homebrew tap for `krill-cli`.
- **Existing MinIO instances.** New S3 servers run Silo, a maintained MinIO fork, since
  v0.7.0; instances created earlier keep the pinned `quay.io/minio/minio` release, which
  receives no updates. Moving one onto Silo in place has not been documented or verified.
- **Existing overlay networks keep their MTU.** `KRILL_OVERLAY_MTU` applies only to networks
  created after it is set; recreating existing ones is a manual procedure
  ([Overlay MTU](docs/architecture.md#overlay-mtu)), not automated.
- A per-owner cap on organizations: each new organization changes the gateway's networks and
  restarts the Traefik task (at most once a minute).
- **Observability, stage 3 (traces):** traces over OTLP, building on the same node agent.
- **Move the Docker client off `github.com/docker/docker` v28** onto the split
  `github.com/moby/moby/client` + `github.com/moby/moby/api` modules (the client negotiates
  down to API 1.40). All five advisories govulncheck reports against v28.5.2 are in daemon
  code, which the client and api modules do not contain, so none of them apply there; on
  the old path they have no fix — two of them,
  [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) and
  [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883), have no symbol list, so the client
  import counts as calling them. Real exposure is the host's Docker Engine (fixed in 29.3.1
  and 29.5.1). Scope: 25 files (9 non-test), mostly `internal/docker` (Swarm, exec, logs,
  stats, one-off containers, SSH remote clients), plus `cmd/krill` and integration tests.

## Later

- Federation across data centers with GeoDNS, as disaster recovery — strictly after
  the resilience work.
- OpenSearch as a managed engine and env-scoped indices; a domain for the S3 console;
  backups for Valkey and DragonFly.
- Encrypted node-to-node traffic (WireGuard); database replication.
- GitHub OAuth / GitHub App instead of webhook + token; SSH deploy keys.
- **Landing page:** a public site for Krill (what it is, screenshots, quick start, links
  to the docs).
- **Signed releases:** sign `checksums.txt` (ed25519) and verify it in the installer and
  the self-updater.
- **Installer-required marker:** a machine-readable release asset so the Updates page can
  refuse a release that needs `install.sh`.

## Not planned for now

Docker Compose, MySQL / MariaDB / MongoDB, buildpacks, preview deployments, custom
RBAC, 2FA and SSO.

## Application metrics (observability stage 2)

Implemented in code: per-app endpoints and encrypted bearer tokens, deployment-time injection, protection of metrics paths on public domains, an organization-network Alloy collector, and live scrape status on the Metrics tab. See [Application metrics](docs/guides/metrics.md).

Disposable two-node live acceptance passed with four application replicas, per-node labels, token rotation and recovery, public metrics-path protection, network cooldown, and firewall isolation. Rolled out to a production install on 2026-09-22: one application collected (`up`), the token enforced (401 without it), the collector's own memory measured by `anon`. Hiding the metrics path on a domain that serves the metrics port is still open (live-acceptance queue, item 13).
