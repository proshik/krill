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
- **Projects and tenancy:** organization → project → environment → app, owner /
  admin / member roles, an instance-admin flag for cluster-wide resources, a
  private overlay network per organization, and self-service password changes
  (forced for anyone newly added to an organization).
- **Managed databases:** org-level Postgres, Redis, DragonFly and MinIO instances;
  logical Postgres databases per environment; connection values injected into app
  env vars; external ports; Postgres backups to S3 with schedule presets, retention
  and restore.
- **Routing:** multiple domains per app with per-domain Let's Encrypt, internal by
  default, per-domain public-path allow-lists, basic auth and IP allow-lists.
- **Operations:** live structured logs, web terminal, CPU/memory monitoring across
  all nodes, Telegram alerts, a visual cluster topology page.
- **Multi-node:** worker nodes joined over SSH, placement (any / pinned / global),
  database instances pinned to a node, firewall lockdown of every node, moving a
  database instance's volume between nodes.
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

## Live-acceptance queue

Verified in code and tests, but not yet exercised on a real environment:

1. MinIO and DragonFly instances on a VPS, including S3 app-linking.
2. Node-down reconciliation — a live drain/remove on a cluster.
3. Cluster-wide monitoring on two nodes (most likely already passed; to confirm).
4. Moving a database instance's volume between nodes on a real cluster, including
   a node failing mid-transfer.
5. The agent API with a real MCP client and a real CI job.
6. `krill-cli` end to end against a real Krill and a real registry.
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
11. Self-update on a real VPS from a real `proshik/krill` release. Everything else in the flow was accepted on
    2026-09-15 on a disposable Lima VM (Ubuntu 24.04, real systemd, `install.sh`) against throwaway releases
    (`make acceptance-selfupdate`): update, manual rollback, crash-loop automatic rollback, a deploy started
    during the download aborting the update, `install.sh` over a UI update, SIGTERM during a migration, and
    the documented recovery from a failed migration. Still open: the starting page through the panel domain
    (Traefik 502/504 while Krill restarts) and a reboot inside the 10-minute rollback window.
12. The observability agent on a real two-node cluster: the agent appears on a node added
    later, host metrics and container logs with `krill_*` labels reach a real Prometheus/Mimir
    and Loki, memory of the agent under load, Check against Mimir with authentication, a worker
    down or drained during a settings change (global update with parallelism 1), an authenticated
    receiver (the 0400 root-owned secret is readable by Alloy), and memory over a long window (it
    was still rising at 14 minutes).

## Open

- **Resilience on top of Swarm:** a resilient ingress (Traefik is a single replica
  pinned to the manager) and a decision on high availability vs. a documented
  fast-recovery runbook for the control plane.
- **Backups of Krill's own state database** — the bundled `krill-postgres` is not
  backed up automatically today.
- **Direct image upload** in `krill-cli` (`delivery: upload`) for users without a
  registry — designed, not implemented; the CLI refuses the mode for now.
- **Concurrent deploys of one app with different tags** can still race, because the
  deploy worker reads the tag when the job starts; the fix is to carry the tag in
  the job itself.
- **Email and Slack** notification channels (Telegram ships today).
- A Homebrew tap for `krill-cli`.
- Recording API-triggered deploys, and the redeploys made by the network migration, as their own trigger types.
- **Network migration: do not let one organization hold back the rest.** Organizations are
  migrated one after another under a single deadline that includes deploy completion, so a
  slow organization delays every organization after it until the next restart. Submit every
  organization's redeploys first, then wait for all of them.
- **Reload of a stopped app after the network migration** restarts it on its old service
  spec, i.e. on the shared network and away from its databases. Reload of a stopped app
  should go through a full deploy.
- **MinIO no longer publishes community builds.** Managed MinIO instances run a pinned
  `quay.io/minio/minio` release that will not receive updates, security fixes included.
  Decide between an actively maintained S3-compatible engine and building MinIO from source.
- A per-owner cap on organizations: each new organization changes the gateway's networks and
  restarts the Traefik task (at most once a minute).
- **Observability, stage 2 (app metrics):** a manager-side collector on every organization's
  network scraping per-app metrics endpoints, gated by a scrape token — the node agent (stage
  1) only covers host metrics and logs.
- **Observability, stage 3 (traces):** traces over OTLP, building on the same node agent.

## Later

- Federation across data centers with GeoDNS, as disaster recovery — strictly after
  the resilience work.
- OpenSearch as a managed engine; env-scoped buckets and indices; a domain for the
  MinIO console; backups for MinIO and DragonFly.
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
