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

{{< landing/why id="self-hosted" title="Your servers, your data"
  lead="Krill runs on a Linux server you own and leaves almost all of it to your apps."
>}}
{{< landing/figures >}}
{{< landing/figure value="Hundreds of MB" label="of RAM for the control plane alone, on a typical self-hosted PaaS, before your first app" >}}
{{< landing/figure value="~35 MB" label="of RAM for the Krill process, measured idle on a small test install" >}}
{{< /landing/figures >}}
<p class="krill-why__note">The Krill binary itself is about 38 MB (linux-amd64) or 34 MB (linux-arm64) in release v0.13.4.</p>
{{< landing/points >}}
{{< landing/detail title="Any Linux server" >}}
amd64 or arm64, with root access. Krill is built to fit a box as small as 2 vCPU / 2 GB RAM.
{{< /landing/detail >}}
{{< landing/detail title="Your data stays yours" >}}
State lives in Postgres on your server or a managed Postgres you choose; backups go to your S3;
metrics and logs go to your own Prometheus and Loki.
{{< /landing/detail >}}
{{< landing/detail title="Standard parts" >}}
Docker Swarm schedules, Traefik routes, Postgres stores. Your apps are ordinary container images.
{{< /landing/detail >}}
{{< landing/detail title="Open source" >}}
MIT licensed, on [GitHub](https://github.com/proshik/krill). One compiled Go binary, with no
Node.js, no SSR runtime and no agents on your servers.
{{< /landing/detail >}}
{{< /landing/points >}}
{{< /landing/why >}}

{{< landing/pillar id="deploy" title="Ship from Git or an image"
  lead="Point an app at a Git repository with a Dockerfile, or at a public or private image. Krill builds it, rolls it out and keeps the history."
  link="https://github.com/proshik/krill/blob/master/docs/guides/deployments.md"
  shot="deployments" w="784" h="1374" side="true"
  alt="An app's deployments: the active release and the history, including an update Swarm rolled back"
>}}
{{< landing/detail title="Zero-downtime updates" >}}
Rolling updates, with an automatic rollback when the new version does not come up.
{{< /landing/detail >}}
{{< landing/detail title="Roll back a release" >}}
Pick a confirmed release in the history: its image and runtime settings come back as a new
deployment.
{{< /landing/detail >}}
{{< landing/detail title="Deploy on push" >}}
A GitHub webhook, a deploy hook for your CI, or `krill-cli` to build on your machine.
{{< /landing/detail >}}
{{< /landing/pillar >}}

{{< landing/pillar id="databases" title="Databases next to your apps"
  lead="Run Postgres, Redis, Valkey, DragonFly or S3 (Silo) on your own nodes. One Postgres server holds a separate database for each environment."
  link="https://github.com/proshik/krill/blob/master/docs/guides/backups.md"
  shot="db-servers" w="1400" h="480"
  alt="Database servers: a Postgres server holding databases for several projects and environments, a Redis server and an S3 server"
>}}
{{< landing/detail title="Connections injected" >}}
Link an app to a database or bucket, and its connection values reach the app on every deploy.
{{< /landing/detail >}}
{{< landing/detail title="Buckets with their own keys" >}}
Per-environment buckets on an S3 server, each with a key restricted to that bucket.
{{< /landing/detail >}}
{{< landing/detail title="Backups to S3" >}}
Scheduled Postgres and volume backups with retention, restored from the UI.
{{< /landing/detail >}}
{{< /landing/pillar >}}

{{< landing/pillar id="networking" title="Domains and HTTPS without the wiring"
  lead="Apps reach each other on their organization's private network. Give an app a domain when it should be public."
  link="https://github.com/proshik/krill/blob/master/docs/guides/domains.md"
  shot="domains" w="1400" h="690"
  alt="An app's domains: its primary domain, internal until exposed, with public paths and HTTPS"
>}}
{{< landing/detail title="A certificate per domain" >}}
Custom domains with their own Let's Encrypt certificates.
{{< /landing/detail >}}
{{< landing/detail title="Internal until exposed" >}}
Publish only some paths, or put a domain behind basic auth and an IP allowlist.
{{< /landing/detail >}}
{{< landing/detail title="TCP and UDP ports" >}}
Raw published ports for what is not HTTP.
{{< /landing/detail >}}
{{< /landing/pillar >}}

{{< landing/pillar id="servers" title="Grow from one server to several"
  lead="Start on one box. When it gets tight, join more servers over SSH and decide what runs where."
  link="https://github.com/proshik/krill/blob/master/docs/architecture.md#nodes"
  shot="topology" w="1400" h="1250" crop="16/9"
  alt="Topology: the ingress lane and a node with its apps and database servers, linked to the databases they use"
>}}
{{< landing/detail title="Placement and replicas" >}}
Pin an app to a node or run it on every node, with as many replicas as it needs.
{{< /landing/detail >}}
{{< landing/detail title="Move a database" >}}
Move a database server to another node together with its data.
{{< /landing/detail >}}
{{< landing/detail title="Locked-down nodes" >}}
Lock every node down with a firewall managed from the UI.
{{< /landing/detail >}}
{{< /landing/pillar >}}

{{< landing/pillar id="observe" title="See what is running"
  lead="Logs, a terminal and resource charts are in the UI from the first deploy. Alerts come to Telegram."
  link="https://github.com/proshik/krill/blob/master/docs/guides/notifications.md"
  shot="monitoring" w="1400" h="460"
  alt="Monitoring: CPU and memory charts per node"
>}}
{{< landing/detail title="Live logs and a terminal" >}}
Structured live logs with search and level filters, and a web terminal into a running container.
{{< /landing/detail >}}
{{< landing/detail title="Alerts and a daily digest" >}}
Failures, successful deploys, node disk and memory thresholds, and a daily summary in Telegram.
{{< /landing/detail >}}
{{< landing/detail title="Your own metrics stack" >}}
An optional Grafana Alloy agent ships host metrics and container logs to your Prometheus and
Loki. Krill keeps none of it.
{{< /landing/detail >}}
{{< /landing/pillar >}}

{{< landing/pillar id="teams" title="Environments for every stage"
  lead="Organize apps into projects and environments, clone production into staging, and give each person the role they need."
  link="https://github.com/proshik/krill/blob/master/docs/guides/environments.md"
  shot="app" w="1400" h="1187" crop="16/10"
  alt="An app's panel over the canvas: the databases, Redis server and bucket linked into its environment variables"
>}}
{{< landing/detail title="Clone an environment" >}}
Copies the configuration, not the data; secret values are left empty in the copy.
{{< /landing/detail >}}
{{< landing/detail title="Roles and isolation" >}}
Owner, admin and member roles. Every organization runs on its own network, and stored
credentials are encrypted at rest.
{{< /landing/detail >}}
{{< landing/detail title="An API for agents and CI" >}}
A REST API and an MCP server with organization-scoped tokens, so an AI agent or a pipeline can
deploy and read logs.
{{< /landing/detail >}}
{{< /landing/pillar >}}

{{< landing/list id="more" title="Also built in" >}}
{{< landing/detail title="Container settings" >}}
CPU and memory limits, healthchecks, volumes, command override, build arguments and secrets.
{{< /landing/detail >}}
{{< landing/detail title="Backups of Krill itself" >}}
Encrypted backups of Krill's own configuration database, scheduled and before every update.
[Krill backups](https://github.com/proshik/krill/blob/master/docs/guides/krill-backups.md)
{{< /landing/detail >}}
{{< landing/detail title="Self-update" >}}
Install a new release from Settings → Updates; a stuck restart rolls back to the previous binary
automatically.
{{< /landing/detail >}}
{{< landing/detail title="Systems" >}}
Group an environment's apps by product and see which resources each group uses.
[Systems](https://github.com/proshik/krill/blob/master/docs/guides/systems.md)
{{< /landing/detail >}}
{{< /landing/list >}}

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
