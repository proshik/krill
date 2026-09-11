# Krill roadmap

What is done, what is waiting for live verification, and what is open.
[README.md](README.md) describes the features in detail.

## Done

- **Apps:** deploy from an image or a Dockerfile (Git clone + local build), deployment
  history, zero-downtime rolling updates, advanced container settings (limits,
  replicas, restart policy, healthcheck, command override), lifecycle controls
  (deploy / reload / rebuild / stop), volumes with S3 backup and restore, raw TCP/UDP
  ports, build args and secrets, private Git and private registries.
- **Projects and tenancy:** organization → project → environment → app, owner /
  admin / member roles, and an instance-admin flag for cluster-wide resources.
- **Managed databases:** org-level Postgres, Redis, DragonFly and MinIO instances;
  logical Postgres databases per environment; connection values injected into app
  env vars; external ports; Postgres backups to S3 with schedule presets, retention
  and restore.
- **Routing:** multiple domains per app with per-domain Let's Encrypt, internal by
  default, per-domain public-path allow-lists, basic auth and IP allow-lists.
- **Operations:** live structured logs, web terminal, CPU/memory monitoring across
  all nodes, Telegram alerts, a visual cluster topology page.
- **Multi-node:** worker nodes joined over SSH, placement (any / pinned / global),
  database instances pinned to a node, firewall lockdown of workers, moving a
  database instance's volume between nodes.
- **Automation:** GitHub push webhook and CI deploy hook, an agent API (REST +
  MCP) with org-scoped tokens, and `krill-cli` to build locally and deploy.
- **Install:** one-line `install.sh` (host binary + systemd), optionally against an
  external managed Postgres.

## Live-acceptance queue

Verified in code and tests, but not yet exercised on a real environment:

1. MinIO and DragonFly instances on a VPS, including S3 app-linking.
2. Node-down reconciliation — a live drain/remove on a cluster.
3. Cluster-wide monitoring on two nodes (most likely already passed; to confirm).
4. Moving a database instance's volume between nodes on a real cluster, including
   a node failing mid-transfer.
5. The agent API with a real MCP client and a real CI job.
6. `krill-cli` end to end against a real Krill and a real registry.
7. `install.sh` downloading a published release (no release has been cut yet).

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
- Recording API-triggered deploys as their own trigger type.

## Later

- Federation across data centers with GeoDNS, as disaster recovery — strictly after
  the resilience work.
- OpenSearch as a managed engine; env-scoped buckets and indices; a domain for the
  MinIO console; backups for MinIO and DragonFly.
- Encrypted node-to-node traffic (WireGuard); database replication.
- GitHub OAuth / GitHub App instead of webhook + token; SSH deploy keys.

## Not planned for now

Docker Compose, MySQL / MariaDB / MongoDB, buildpacks, preview deployments, custom
RBAC, 2FA and SSO.
