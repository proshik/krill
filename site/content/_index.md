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
  video="hero" vw="1600" vh="1100"
  videoalt="Krill in use: a new app is created from an image, deployed with a live log, exposed on its domain and opened in the browser"
>}}

{{< landing/why id="self-hosted" title="Your servers, your data"
  lead="Krill runs on a Linux server you own and leaves almost all of it to your apps."
>}}
{{< landing/figures >}}
{{< landing/figure value="Hundreds of MB" label="of RAM for the control plane alone, on a typical self-hosted PaaS, before your first app" >}}
{{< landing/figure value="~34 MB" label="of RAM for the Krill process, measured idle on a fresh install on a 2 vCPU / 2 GB Linux server" >}}
{{< /landing/figures >}}
<p class="krill-why__note">Krill itself is one compiled Go binary of under 40 MB.</p>
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

{{< landing/flow id="how-it-works" title="How it works"
  lead="One Go binary runs the control plane. Docker Swarm runs your workloads, and Traefik routes traffic to them."
  link="https://github.com/proshik/krill/blob/master/docs/architecture.md#overview"
>}}
{{< landing/step title="You" note="git push" >}}
The web UI, a REST API and an MCP server, for people, CI and AI agents.
{{< /landing/step >}}
{{< landing/step title="Krill" note="Docker API" >}}
One Go binary on your server. It keeps its state in Postgres and runs the deploy queue,
schedules and alerts.
{{< /landing/step >}}
{{< landing/step title="Docker Swarm" note="routed by Traefik" >}}
Your apps and databases as Swarm services, each organization on its own overlay network, on one
node or several joined over SSH.
{{< /landing/step >}}
{{< landing/step title="Your domains" >}}
Traefik routes each request by host, with a Let's Encrypt certificate for each domain.
{{< /landing/step >}}
{{< /landing/flow >}}

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
Custom domains with their own Let's Encrypt certificates, the Krill UI's included: it moves to
its domain only after you confirm it works, so a broken domain cannot lock you out.
{{< /landing/detail >}}
{{< landing/detail title="Internal until exposed" >}}
Publish only some paths, or put a domain behind basic auth and an IP allowlist.
{{< /landing/detail >}}
{{< landing/detail title="Several apps on one host" >}}
Share a host between apps by path, and redirect an old domain to the app's primary one.
{{< /landing/detail >}}
{{< /landing/pillar >}}

{{< landing/pillar id="servers" title="Grow from one server to several"
  lead="Start on one box. When it gets tight, join more servers over SSH and decide what runs where. The topology map shows which app runs on which node and which database it talks to."
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
Loki, and the [Metrics tab](https://github.com/proshik/krill/blob/master/docs/guides/metrics.md)
collects your apps' own Prometheus metrics. Krill keeps none of it.
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
CPU and memory limits, healthchecks, volumes, command override, raw TCP and UDP ports, build
arguments and secrets.
{{< /landing/detail >}}
{{< landing/detail title="Private images and repositories" >}}
Registry credentials for private images and access tokens for private Git repositories, each
bound to its own host.
[Private images and repositories](https://github.com/proshik/krill/blob/master/docs/guides/registries.md)
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
{{< landing/detail title="English and Russian" >}}
The interface is in English and Russian, chosen in Settings or taken from the browser's language.
{{< /landing/detail >}}
{{< /landing/list >}}

{{< landing/faq id="faq" title="Questions" >}}
{{< landing/qa q="What server do I need?" >}}
A Linux server, amd64 or arm64, with root access; Krill is built to fit a box as small as
2 vCPU / 2 GB RAM. Ports 80 and 443 carry app traffic, and 8080 serves the UI until it has a
domain. [Requirements](https://github.com/proshik/krill/blob/master/docs/install.md#requirements)
{{< /landing/qa >}}
{{< landing/qa q="What does it cost?" >}}
Nothing: Krill is open source under the
[MIT license](https://github.com/proshik/krill/blob/master/LICENSE). You pay only for the servers
you run it on.
{{< /landing/qa >}}
{{< landing/qa q="Where does Krill keep its state?" >}}
In Postgres: a local container the installer sets up, or a managed Postgres you point it at with
`KRILL_DATABASE_URL`. Krill can also back that database up, encrypted, to your S3.
[External Postgres](https://github.com/proshik/krill/blob/master/docs/install.md#use-a-managed--external-postgres)
{{< /landing/qa >}}
{{< landing/qa q="How are secrets and teams kept apart?" >}}
Stored credentials are encrypted at rest with AES-256-GCM under a key the installer generates.
Every organization runs on its own network, apps stay internal until you expose them, and nodes
can be locked down with a firewall.
[Networking and tenancy](https://github.com/proshik/krill/blob/master/docs/architecture.md#networking-and-tenancy)
{{< /landing/qa >}}
{{< landing/qa q="What happens to my apps if Krill stops?" >}}
They keep running: apps are Docker Swarm services, and Traefik reads their routes from Swarm
itself. The UI, the API, deploys, backups and alerts wait until Krill is back. There is no high
availability: if the server that runs Krill goes down, the UI and the ingress go with it.
[Architecture](https://github.com/proshik/krill/blob/master/docs/architecture.md#overview)
{{< /landing/qa >}}
{{< landing/qa q="How do I update it?" >}}
From Settings → Updates, or by running the installer again. If a new release does not come up
within 10 minutes, the previous binary is put back automatically.
[Upgrade](https://github.com/proshik/krill/blob/master/docs/install.md#upgrade)
{{< /landing/qa >}}
{{< landing/qa q="How do I remove it?" >}}
Stop and remove the systemd service and the binary, then Krill's own Postgres container. The apps,
databases and networks Krill created stay until you delete them.
[Uninstall](https://github.com/proshik/krill/blob/master/docs/install.md#uninstall)
{{< /landing/qa >}}
{{< /landing/faq >}}

{{< landing/quickstart title="Quick start" actions="true" >}}
On a fresh Linux server (amd64 or arm64):

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
```

The installer sets up Docker and Swarm, Postgres for Krill's state and a systemd service,
verifies the downloaded binary against the release checksums, and prints the URL and a one-time
admin password. Options, external Postgres and uninstalling are covered in
[Installing Krill](https://github.com/proshik/krill/blob/master/docs/install.md).
{{< /landing/quickstart >}}
