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

{{< landing/phases id="phases" title="From a fresh VPS to a cluster"
  lead="Each step as it looks in Krill. Pick one, or let them play in turn."
>}}
{{< landing/phase id="install" label="Install" icon="download" video="install"
  title="One command on any VPS"
  lead="Run the installer on a fresh Linux server. It sets up Docker and Swarm, Postgres for Krill's state and a systemd service, then prints the URL and a one-time password."
  link="https://github.com/proshik/krill/blob/master/docs/install.md"
  alt="A terminal replays a real install.sh run, then the Krill sign-in page opens"
>}}
{{< landing/detail title="Small servers welcome" >}}
amd64 or arm64; Krill is built to fit a box as small as 2 vCPU / 2 GB RAM.
{{< /landing/detail >}}
{{< landing/detail title="Verified binary" >}}
The download is checked against the release checksums before it runs.
{{< /landing/detail >}}
{{< landing/detail title="Upgrade the same way" >}}
Run the installer again, or update from Settings → Updates.
{{< /landing/detail >}}
{{< /landing/phase >}}
{{< landing/phase id="deploy" label="Deploy" icon="cube" video="deploy"
  title="Ship from Git or an image"
  lead="Point an app at a public or private image, or at a Git repository with a Dockerfile. Krill rolls it out with a live log and keeps the history."
  link="https://github.com/proshik/krill/blob/master/docs/guides/deployments.md"
  alt="A new app is created from the nginx image, deployed with a live log, exposed on its domain and opened in the browser"
>}}
{{< landing/detail title="Zero-downtime updates" >}}
Rolling updates, with an automatic rollback when the new version does not come up.
{{< /landing/detail >}}
{{< landing/detail title="Roll back a release" >}}
Pick a confirmed release in the history: its image and runtime settings come back.
{{< /landing/detail >}}
{{< landing/detail title="Deploy on push" >}}
A GitHub webhook, a deploy hook for your CI, or `krill-cli` to build on your machine.
{{< /landing/detail >}}
{{< /landing/phase >}}
{{< landing/phase id="connect" label="Connect" icon="database" video="connect"
  title="Databases next to your apps"
  lead="Create a Postgres database, a Redis, Valkey or DragonFly server, or an S3 bucket, and link it to an app: its connection values reach the app on every deploy."
  link="https://github.com/proshik/krill/blob/master/docs/guides/backups.md"
  alt="A database is created in the environment and linked into an app's variables; the canvas draws the link"
>}}
{{< landing/detail title="A database per environment" >}}
One Postgres server holds a separate database for each environment.
{{< /landing/detail >}}
{{< landing/detail title="Buckets with their own keys" >}}
Per-environment buckets on an S3 server, each with a key restricted to that bucket.
{{< /landing/detail >}}
{{< landing/detail title="Backups to S3" >}}
Scheduled Postgres and volume backups with retention, restored from the UI.
{{< /landing/detail >}}
{{< /landing/phase >}}
{{< landing/phase id="expose" label="Expose" icon="globe-alt" video="expose"
  title="Public when you say so"
  lead="Apps stay internal until you expose a domain. Add your own domains with Let's Encrypt certificates, and protect them with basic auth or an IP allowlist."
  link="https://github.com/proshik/krill/blob/master/docs/guides/domains.md"
  alt="A domain is added to an app and protected with basic auth; curl gets 401 without the password and 200 with it"
>}}
{{< landing/detail title="A certificate per domain" >}}
The Krill UI's own domain too: it moves there only after you confirm it works.
{{< /landing/detail >}}
{{< landing/detail title="Several apps on one host" >}}
Share a host by path, or redirect an old domain to the app's primary one.
{{< /landing/detail >}}
{{< landing/detail title="TCP and UDP ports" >}}
Raw published ports for what is not HTTP.
{{< /landing/detail >}}
{{< /landing/phase >}}
{{< landing/phase id="grow" label="Grow" icon="server" video="grow"
  title="From one server to several"
  lead="Join more servers over SSH and decide what runs where. The topology map shows which app runs on which node and which database it talks to."
  link="https://github.com/proshik/krill/blob/master/docs/architecture.md#nodes"
  alt="The Nodes page with its add-node form, the topology map, and a database server's node setting"
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
{{< /landing/phase >}}
{{< landing/phase id="observe" label="Observe" icon="chart-bar" video="observe"
  title="See what is running"
  lead="Live logs with search and level filters, CPU and memory per node, and Telegram alerts with a daily digest."
  link="https://github.com/proshik/krill/blob/master/docs/guides/notifications.md"
  alt="Live logs of an app under real traffic filtered to errors, the monitoring charts, and the Telegram notification settings"
>}}
{{< landing/detail title="A web terminal" >}}
A shell in a running container, from the browser.
{{< /landing/detail >}}
{{< landing/detail title="Your own metrics stack" >}}
An optional Grafana Alloy agent ships host metrics, container logs and your apps' own metrics to
your Prometheus and Loki. Krill keeps none of it.
{{< /landing/detail >}}
{{< landing/detail title="Alerts that matter" >}}
Failures, successful deploys, node disk and memory thresholds, and a daily summary.
{{< /landing/detail >}}
{{< /landing/phase >}}
{{< /landing/phases >}}

{{< landing/list id="more" title="Also built in" >}}
{{< landing/detail title="Environments" >}}
Projects with production, staging and more; clone an environment's configuration, not its data,
with secret values left empty. [Environments](https://github.com/proshik/krill/blob/master/docs/guides/environments.md)
{{< /landing/detail >}}
{{< landing/detail title="Teams and isolation" >}}
Owner, admin and member roles. Every organization runs on its own network; stored credentials
are encrypted at rest.
{{< /landing/detail >}}
{{< landing/detail title="An API for agents and CI" >}}
A REST API and an MCP server with organization-scoped tokens.
[Agent and CI API](https://github.com/proshik/krill/blob/master/docs/guides/agent-api.md)
{{< /landing/detail >}}
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
