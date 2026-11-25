---
title: Krill
layout: hextra-home
description: A lightweight self-hosted PaaS on Docker Swarm. Deploy apps and managed databases to your own servers from a single Go binary.
---

{{< landing/hero
  title="A lightweight <span class=\"krill-nowrap\">self-hosted</span> PaaS on Docker Swarm"
  subtitle="Deploy apps and managed databases to your own servers from a single Go binary."
  shot="overview" w="2000" h="1376"
  alt="A Krill environment canvas: apps grouped into systems, wired to the Postgres databases, Redis server and S3 bucket they use"
>}}

{{< landing/why title="Why Krill" >}}
{{< landing/figures >}}
{{< landing/figure value="760 MB" label="of RAM that Dokploy alone took on a 2 vCPU / 2 GB VPS, more than any app it was running" >}}
{{< landing/figure value="20 MB" label="Krill: one compiled Go binary, with no Node.js, no SSR runtime and no agents on your servers" >}}
{{< /landing/figures >}}
<p class="krill-why__note">Krill uses Docker Swarm for scheduling and Traefik for routing, and stores its state in Postgres.</p>
{{< /landing/why >}}

{{< landing/feature id="databases" title="Managed databases" layout="showcase"
  shot="db-servers" w="1400" h="480"
  alt="Database servers: a Postgres server holding databases for several projects and environments, a Redis server and an S3 server"
>}}
Postgres, Redis, Valkey, DragonFly and S3 (Silo) with per-environment buckets and restricted keys.
One Postgres server holds separate databases for each environment.
{{< /landing/feature >}}

{{< landing/feature id="linking" title="Linked, not copied" layout="split-reverse"
  shot="app" w="1400" h="1187"
  alt="An app's panel over the canvas: the databases, Redis server and bucket linked into its environment variables"
>}}
Linked apps get connection values injected on every deploy: a database URL, a Redis server, a
bucket's endpoint and restricted key.
{{< /landing/feature >}}

{{< landing/feature id="domains" title="Domains and HTTPS" layout="showcase"
  shot="domains" w="1400" h="690"
  alt="An app's domains: its primary domain, internal until exposed, with public paths and HTTPS"
>}}
Custom domains with per-domain Let's Encrypt certificates. Apps are internal until you expose
them, and a domain can publish only some paths or sit behind basic auth and an IP allowlist.
[Domains, HTTPS and access control](https://github.com/proshik/krill/blob/master/docs/guides/domains.md)
{{< /landing/feature >}}

{{< landing/feature id="servers" title="Multiple servers" layout="split"
  shot="topology" w="1400" h="1250"
  alt="Topology: the ingress lane and a node with its apps and database servers, linked to the databases they use"
>}}
Join worker nodes over SSH, pin apps to nodes or run them everywhere, move a database to another
node with its data, lock every node down with a firewall. The topology map shows which app runs
where and talks to which database.
{{< /landing/feature >}}

{{< landing/feature id="operations" title="Operations" layout="showcase"
  shot="monitoring" w="1400" h="460"
  alt="Monitoring: CPU and memory charts per node"
>}}
Structured live logs, a web terminal, CPU and memory charts per node, and Telegram alerts
(failures, successful deploys, node disk and memory thresholds) with a daily digest.
[Notifications](https://github.com/proshik/krill/blob/master/docs/guides/notifications.md)
{{< /landing/feature >}}

{{< landing/also title="Also in Krill" >}}
{{< landing/item title="Images or Dockerfiles" >}}
Build from a Git repository on the server, or pull a public or private image. Zero-downtime
rolling updates, automatic rollback, deployment history with live build logs.
{{< /landing/item >}}
{{< landing/item title="Backups to S3" >}}
Scheduled Postgres and volume backups with retention, restored from the UI.
{{< /landing/item >}}
{{< landing/item title="Backups of Krill itself" >}}
Encrypted backups of Krill's own configuration database, scheduled and before every update.
{{< /landing/item >}}
{{< landing/item title="Container settings" >}}
CPU and memory limits, replicas, healthchecks, volumes, command override, raw TCP/UDP ports,
build arguments and secrets.
{{< /landing/item >}}
{{< landing/item title="Observability agent" >}}
An optional Grafana Alloy agent ships host metrics and container logs to your own
Prometheus-compatible storage and Loki. Krill keeps none of it.
{{< /landing/item >}}
{{< landing/item title="Automation" >}}
GitHub push-to-deploy, a CI deploy hook, a REST API and an MCP server for AI agents, and
`krill-cli` to build locally and deploy.
{{< /landing/item >}}
{{< landing/item title="Teams" >}}
Organizations, projects and environments with owner, admin and member roles. Stored credentials
are encrypted at rest.
{{< /landing/item >}}
{{< landing/item title="Self-update" >}}
Install a new release from Settings → Updates; a stuck restart rolls back to the previous binary
automatically.
{{< /landing/item >}}
{{< /landing/also >}}

{{< landing/quickstart title="Quick start" >}}
On a fresh Linux server (amd64 or arm64):

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
```

The installer sets up Docker and Swarm, Postgres for Krill's state and a systemd service,
verifies the downloaded binary against the release checksums, and prints the URL and a one-time
admin password. Options, external Postgres and uninstalling are covered in
[Installing Krill](https://github.com/proshik/krill/blob/master/docs/install.md).
{{< /landing/quickstart >}}
