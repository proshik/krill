<div align="center">

<img src="internal/web/static/favicon.svg" alt="Krill logo" width="96" height="96">

# Krill

**A lightweight self-hosted PaaS on Docker Swarm.**<br>
Deploy apps and managed databases to your own servers from a single Go binary.

[![CI](https://github.com/proshik/krill/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/proshik/krill/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/proshik/krill)](https://github.com/proshik/krill/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/proshik/krill)](go.mod)
[![License: MIT](https://img.shields.io/github/license/proshik/krill)](LICENSE)

[Install](docs/install.md) · [Documentation](#documentation) · [Roadmap](ROADMAP.md)

</div>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/overview-dark.png">
  <img src="docs/images/overview-light.png" alt="A Krill environment canvas: apps grouped into systems, wired to the Postgres databases, Redis server and S3 bucket they use">
</picture>

## Why Krill

Krill started as a way to host a Telegram bot and a few small apps on a 2 vCPU / 2 GB VPS.
Existing self-hosted platforms do the job, but their control planes are heavy: Dokploy alone
took about **760 MB** of RAM on that box, more than any app it was running.

Krill is the same idea at a fraction of the weight: one compiled Go binary of under 40 MB, with
no Node.js, no SSR runtime and no agents on your servers. It uses Docker Swarm for scheduling
and Traefik for routing, and stores its state in Postgres.

## Features

- **Deploy from an image or a Dockerfile.** Build from a Git repository on the server, or pull
  a public or private image. Zero-downtime rolling updates, automatic rollback, deployment
  history with live build logs.
- **Managed databases.** Postgres, Redis, Valkey, DragonFly and S3 (Silo) with per-environment buckets and restricted keys. One Postgres server holds
  separate databases for each environment, and linked apps get connection values injected on
  every deploy.
- **Backups to S3.** Scheduled Postgres and volume backups with retention, restored from the
  UI. → [Backups](docs/guides/backups.md)
- **Backups of Krill itself.** Encrypted backups of Krill's own configuration database, scheduled and
  before every update. → [Krill backups](docs/guides/krill-backups.md)
- **Domains and HTTPS.** Custom domains with per-domain Let's Encrypt certificates. Apps are
  internal until you expose them, and a domain can publish only some paths or sit behind basic
  auth and an IP allowlist. The Krill UI itself gets a domain and HTTPS from its settings
  page, with a confirmation step so a broken domain can't lock you out.
  → [Domains](docs/guides/domains.md)
- **Container settings.** CPU and memory limits, replicas, healthchecks, volumes, command
  override, raw TCP/UDP ports, build arguments and secrets.
- **Operations.** Structured live logs, a web terminal, CPU and memory charts per node,
  Telegram alerts (failures, successful deploys, node disk and memory thresholds) with a daily
  digest, and a map of which app runs where and talks to which database.
  → [Notifications](docs/guides/notifications.md)
- **Observability agent.** An optional Grafana Alloy agent on every node ships host metrics and
  container logs to your own Prometheus-compatible storage and Loki — Krill keeps none of it.
  → [Observability](docs/guides/observability.md)
- **Multiple servers.** Join worker nodes over SSH, pin apps to nodes or run them everywhere,
  move a database to another node with its data, lock every node down with a firewall.
- **Automation.** GitHub push-to-deploy, a CI deploy hook, a REST API and an MCP server for
  AI agents, and `krill-cli` to build locally and deploy. →
  [API](docs/guides/agent-api.md) · [CLI](docs/guides/krill-cli.md)
- **Teams.** Organizations, projects and environments with owner, admin and member roles.
  Every organization runs on its own network; stored credentials are encrypted at rest. The UI
  is in English and Russian.
- **Self-update.** Install a new release from Settings → Updates with one click; a stuck
  restart rolls back to the previous binary automatically.

## Quick start

On a fresh Linux server (amd64 or arm64):

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
```

The installer sets up Docker and Swarm, Postgres for Krill's state and a systemd service,
verifies the downloaded binary against the release checksums, and prints the URL and a
one-time admin password. Install later updates from **Settings → Updates** in the UI, or by
re-running this command.

Then give the UI a domain and HTTPS under **Settings → Panel domain**. Options, external
Postgres and uninstalling are covered in [Installing Krill](docs/install.md).

## Screenshots

<table>
  <tr>
    <th>Topology</th>
    <th>App</th>
  </tr>
  <tr>
    <td>
      <picture>
        <source media="(prefers-color-scheme: dark)" srcset="docs/images/topology-dark.png">
        <img src="docs/images/topology-light.png" alt="Topology: the ingress lane and a node with its apps and database servers, linked to the databases they use">
      </picture>
    </td>
    <td>
      <picture>
        <source media="(prefers-color-scheme: dark)" srcset="docs/images/app-dark.png">
        <img src="docs/images/app-light.png" alt="An app's panel over the canvas: the databases, Redis server and bucket linked into its environment variables">
      </picture>
    </td>
  </tr>
</table>

## Status

Krill is **pre-1.0**. It runs the author's own projects, but it is not yet hardened for anyone
else's production:

- **No high availability.** One control plane on one node; if that node goes down, the UI and
  the ingress go with it.
- **Krill's own database is not backed up automatically.** Back it up yourself, or install
  against a managed Postgres.
- **Some features are verified in tests but not yet on a live cluster.**

[ROADMAP.md](ROADMAP.md) lists what is done, what awaits live verification and what is planned.

## Documentation

| Guide | Covers |
|-------|--------|
| [Installing Krill](docs/install.md) | Install, upgrade, external Postgres, uninstall. |
| [Configuration](docs/configuration.md) | Every `KRILL_*` setting. |
| [Domains, HTTPS and access control](docs/guides/domains.md) | Custom domains, certificates, path exposure, basic auth, IP allowlists, raw ports. |
| [Environment cloning](docs/guides/environments.md) | Isolated staging configuration, secret handling and cleanup. |
| [Deployments](docs/guides/deployments.md) | Pre-deploy commands, failure behavior and limits. |
| [Backups](docs/guides/backups.md) | S3 storage, schedules, retention, restore. |
| [Krill's own backups](docs/guides/krill-backups.md) | Encrypted state backups, the age key, pre-update snapshots, restore runbook. |
| [Systems](docs/guides/systems.md) | Group an environment's apps by product and see the resources each system uses. |
| [Buckets](docs/guides/buckets.md) | Per-environment buckets on a managed S3 server, restricted keys, rotation. |
| [Private images and repositories](docs/guides/registries.md) | Registries, private Git, build arguments and secrets. |
| [Agent and CI API](docs/guides/agent-api.md) | REST, MCP, tokens, webhooks. |
| [Notifications](docs/guides/notifications.md) | Telegram alerts, successful deploys, disk and memory thresholds, the daily digest. |
| [Observability](docs/guides/observability.md) | The optional Grafana Alloy agent: metrics, logs, labels, cost, security. |
| [krill-cli](docs/guides/krill-cli.md) | Build on your machine, deploy to Krill. |
| [Architecture](docs/architecture.md) | How it fits together, data model, source layout. |
| [Development](docs/development.md) | Run from source, tests, releases. |
| [Internals](docs/internals/README.md) | Implementation notes per feature, for contributors and coding agents (rules in [AGENTS.md](AGENTS.md)). |

## Contributing

Bug reports, ideas and pull requests are welcome — start with
[CONTRIBUTING.md](CONTRIBUTING.md). To report a vulnerability, follow
[SECURITY.md](SECURITY.md) rather than opening a public issue.

## License

[MIT](LICENSE)
