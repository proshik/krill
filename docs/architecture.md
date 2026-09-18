# Architecture

- [Overview](#overview)
- [How a deploy works](#how-a-deploy-works)
- [Networking and tenancy](#networking-and-tenancy)
- [Nodes](#nodes)
- [Data model](#data-model)
- [Source layout](#source-layout)
- [Tech stack](#tech-stack)

## Overview

Krill is one Go binary — the control plane. It keeps all of its state in PostgreSQL, drives
Docker Swarm through the Docker API, and leaves HTTP routing to Traefik, which reads the
labels Krill puts on each Swarm service.

```
                ┌────────────────────────────────────────────────┐
   browser ───▶ │  Krill control plane (Go binary, :8080)        │
   agents  ───▶ │   - web UI (templ + HTMX), REST API, MCP       │
   CI      ───▶ │   - state in PostgreSQL                        │
                │   - deploy queue, schedulers, metrics sampler  │
                └───────────────────────┬────────────────────────┘
                                        │ Docker API (local socket; SSH tunnel to workers)
                                        ▼
                ┌────────────────────────────────────────────────┐
                │  Docker Swarm: manager + optional workers       │
                │   - Traefik v3.6.1, host :80 / :443            │
                │   - one overlay network per organization       │
                │   - app services         krill-<appID>         │
                │   - database instances   krill-<engine>-…      │
                │     with named volumes   <name>-data           │
                └───────────────────────┬────────────────────────┘
                                        │ routed by Host header (+ optional paths)
                                        ▼
                                  your domains
```

The control plane is a single process on the manager node; it has no high availability. If
the manager goes down, the UI and the ingress go with it.

The UI can also be served through Traefik on a domain of its own. Krill is a host process,
not a Swarm service, so there are no labels for Traefik to read. Instead Traefik's HTTP
provider polls Krill (`/_krill/gateway/config`, authenticated by a token in the provider's
arguments) for the UI's routes, which point back at `KRILL_ADVERTISE_ADDR:<listen port>`.
The routes live in the database, so they survive restarts and gateway reconciliation and
change without redeploying Traefik. Every request the gateway proxies to the UI carries a
second token, which is how Krill tells a request that came through Traefik over HTTPS (and
whose `X-Forwarded-*` headers Traefik set itself) from one sent straight to the UI port. See
[Serve the Krill UI on a domain](guides/domains.md#serve-the-krill-ui-on-a-domain).

## How a deploy works

1. A deploy is queued — from the UI, a GitHub push webhook, a CI deploy hook, the REST/MCP
   API or `krill-cli`. One worker takes jobs off the queue in order, which keeps Docker API
   calls from contending on a small host. A per-app check allows one deploy in flight at a
   time, and a per-organization cap (`KRILL_MAX_BUILDS_PER_ORG`) keeps one tenant from holding
   the queue.
2. For a Dockerfile app, Krill shallow-clones the repository and runs `docker build` on the
   host — no registry needed. For an image app, it resolves the tag to a digest so a re-pushed
   tag is actually pulled again.
3. Krill creates or updates the Swarm service: image, env (with values from linked databases
   resolved at this moment), limits, healthcheck, volumes, ports, placement and Traefik
   labels. Updates start the new task before stopping the old one and roll back on failure —
   except for a service that publishes a host-mode port, or a database instance, which stop
   the old task first (see [Nodes](#nodes) for why a database needs that).
4. The deploy is marked done only when the **new** tasks are running — tasks that existed
   before the deploy don't count — or failed after `KRILL_CONVERGE_TIMEOUT`.

Build and deploy logs stream to the browser over WebSocket as they happen. A finished
deployment keeps its log for an hour and its record — status, trigger, image, timing — for
good.

## Networking and tenancy

- Resources form a tree: **organization → project → environment → app**. Database instances
  belong to an organization; the logical databases inside a Postgres instance belong to an
  environment.
- Every request loads the whole ownership chain and answers **404** for anything outside the
  caller's organization, never 403, so IDs from other tenants are not disclosed.
- Roles are `owner` > `admin` > `member`. Members can view; admins change things; only an
  owner can grant the owner role. Cluster-wide resources — nodes, the cluster firewall, host
  monitoring — are limited to the **instance administrator**, a separate flag, because anyone
  can create an organization and become its owner.
- Each organization has its own overlay network, `krill-org-<id>`; services in different
  organizations can't resolve or reach each other by name. Traefik joins every organization's
  network.
- New apps are **internal**: nothing is routed to them until a domain is marked exposed.

Network isolation is not a boundary against the instance administrator or against a container
that escapes to the host — see [SECURITY.md](../SECURITY.md) for the trust model.

## Nodes

The Swarm starts with the manager alone. Worker nodes are joined over SSH from
**Settings → Nodes**; apps can run on any node, be pinned to specific nodes, or run on every
node. Stateful services stay node-local: a database instance is pinned to one node together
with its volume, and moving it copies the volume to the new node.

Krill reaches a worker's Docker daemon through an SSH tunnel with the stored key — for
`docker exec` (backups, database provisioning, the web terminal) and for per-node metrics. No
agent runs on the workers and no extra port is opened.

A database instance's data directory is protected against ever running two writers at once —
the failure mode that matters most for a node coming back into the cluster mid-reconciliation,
which is not a service update Swarm's own rolling-update order can see. Every Postgres, Redis
and DragonFly instance wraps its container's own entrypoint in a portable flock around the
volume's mount-point directory (the volume itself, since for Postgres that directory is
`PGDATA`): it opens the directory on a fixed file descriptor and locks that descriptor rather
than a path, a form both GNU/util-linux's and busybox's `flock` understand — busybox, shipped
by every alpine-tagged image (`postgres:16-alpine`, `redis:7-alpine`, and any other alpine tag,
since the image is free text), has no `--no-fork` flag at all and would otherwise fail outright
and crash-loop the instance. A second container placed on the same volume — by Swarm or by hand
— blocks until the first one actually stops, and a clean stop still reaches the database
process instead of turning into a `SIGKILL`. This sits on top of, not instead of, starting
every database instance's Swarm updates stop-first. MinIO's image has neither `sh` nor `flock`,
so it only gets the stop-first protection, not the directory lock — a known, accepted gap. A
custom image missing either tool fails to start.

A database instance has no Reload or Rebuild (those are app-only actions); its own actions are
Deploy, Start, Stop, an image-version change, a node move, and delete. Only Deploy, a
version change, or a node move actually redeploy the instance and pick up this protection —
Start and Stop just scale the existing service and leave whatever spec is already running in
place. An instance already running before this shipped stays exposed to a bad node return until
one of those three happens; Krill does not redeploy every database instance automatically on
upgrade, so redeploy the ones holding real data soon after upgrading.

The optional cluster firewall (**Settings → Firewall**) closes inbound traffic on every node
except SSH, ICMP and Swarm traffic from other cluster nodes; the control plane also keeps HTTP,
HTTPS and the Krill UI port open — or, once the UI's domain is confirmed and direct access
closed, the UI port open only to the gateway (`docker_gwbridge`). A node that becomes unreachable after the change reverts it
on its own, and the control plane's lockdown is reverted unless you confirm it from the page
within two minutes.

## Data model

State lives in PostgreSQL. The schema is created by embedded migrations
([`internal/database/migrations`](../internal/database/migrations)) that run on startup, and
queried through code generated by sqlc ([`internal/database/gen`](../internal/database/gen)).

| Area | Tables |
|------|--------|
| Accounts | `users`, `sessions`, `api_tokens` |
| Tenancy | `organizations`, `members`, `projects`, `environments` |
| Apps | `applications`, `deployments`, `domains`, `app_ports`, `app_volumes`, `app_db_links` |
| Databases | `db_instances`, `logical_databases` |
| Backups | `destinations` (S3 storage), `backups`, `volume_backups` |
| Credentials | `registries`, `git_credentials` |
| Cluster | `cluster_nodes`, `node_labels`, `node_capacity` |
| Operations | `notification_channels`, `metric_samples` |

Deleting a project or an environment removes everything under it, including its logical
databases and their data. A database instance can't be deleted while it still holds logical
databases.

## Source layout

| Path | Responsibility |
|------|----------------|
| `cmd/krill` | Server entry point: config, migrations, wiring, Traefik reconcile, HTTP server. |
| `cmd/krill-cli` | Entry point of the command-line client. |
| `internal/server` | HTTP router, middleware and handlers for the UI, webhooks and REST API. |
| `internal/auth` | Sessions, password hashing, roles and the middleware that enforces them. |
| `internal/org` | Organizations, projects, environments and membership. |
| `internal/deploy` | Deploy queue and worker, convergence, log hub. |
| `internal/builder` | Git clone and `docker build`, with input validation. |
| `internal/docker` | Wrapper over the Docker and Swarm APIs. |
| `internal/traefik` | Traefik service spec and routing labels. |
| `internal/panel` | The Krill UI's own route through the gateway: dynamic configuration, tokens, domain validation. |
| `internal/dbservice` | Database instance lifecycle; per-engine drivers in `drivers/`. |
| `internal/backup` | S3 storage, scheduled `pg_dump` backups and restore. |
| `internal/volume` | App volume validation and backup/restore. |
| `internal/cluster` | Joining worker nodes over SSH. |
| `internal/firewall` | nftables allowlist for cluster nodes. |
| `internal/orgnet` | Per-organization networks and the migration onto them. |
| `internal/metrics` | CPU/memory sampling across nodes. |
| `internal/notify` | Telegram notifications and the health watcher. |
| `internal/topology` | Graph builder for the Topology page. |
| `internal/api` | The agent-facing operations and token authentication. |
| `internal/mcpsrv` | MCP server over `internal/api`. |
| `internal/webhook` | GitHub signature verification and payload parsing. |
| `internal/krillcli` | `krill-cli`: commands, config, API client, deploy flow. |
| `internal/secret` | Encryption of stored secrets. |
| `internal/netguard` | Blocks outbound connections to private addresses (SSRF). |
| `internal/buildinfo` | The binary's version, stamped at build time, and whether it is a release. |
| `internal/selfupdate` | Discovering newer releases, installing one with a dead-man rollback timer, and rolling back. |
| `internal/envtext`, `internal/logparse`, `internal/oplock` | Env-file parsing, log-line parsing, named operation locks. |
| `internal/web` | Templates, static assets, i18n catalogs (English, Russian). |
| `internal/database` | Migrations and generated queries. |
| `internal/testutil` | Throwaway Postgres and MinIO containers for tests. |

## Tech stack

- **Go** 1.26, single static binary (`CGO_ENABLED=0`).
- **HTTP:** chi, coder/websocket.
- **UI:** templ, HTMX, Tailwind CSS v4 through its standalone CLI — no Node.js anywhere in the
  build.
- **Database:** PostgreSQL through pgx, queries generated by sqlc, migrations by
  golang-migrate.
- **Orchestration:** Docker Swarm through the Docker Engine API; Traefik v3.6.1 for ingress
  and Let's Encrypt.
- **Backups:** AWS SDK for Go v2 (S3 and compatible), robfig/cron.
- **Agent API:** the official MCP Go SDK.
- **Tests:** testcontainers-go.
