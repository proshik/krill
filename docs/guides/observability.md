# Observability agent

Krill can run [Grafana Alloy](https://grafana.com/docs/alloy/latest/) on every node and ship
host metrics and container logs to storage you already run — a Prometheus-compatible receiver
and Loki. Krill itself keeps none of that data; it only stores the two push addresses and
their credentials.

- [1. What it does](#1-what-it-does)
- [2. Requirements](#2-requirements)
- [3. Turning it on](#3-turning-it-on)
- [4. Labels](#4-labels)
- [5. Cost](#5-cost)
- [6. Security](#6-security)
- [7. Turning it off / troubleshooting](#7-turning-it-off--troubleshooting)

## 1. What it does

Turning it on deploys `krill-alloy-node`, a **global** Swarm service — one task per node,
including nodes added later. Each task:

- scrapes that node's own host metrics (CPU, memory, disk, network — the standard Unix node
  exporter set) and pushes them via Prometheus remote write;
- tails the Docker logs of **every** container on that node (Krill's own services included) and
  pushes them to Loki.

That is the stdout/stderr of every container on every node — Krill's own state database and
every tenant's apps and database servers included — so the Loki you point it at ends up holding
every tenant's logs. Give it the same care as Krill itself.

Krill renders the agent's whole configuration from the settings you save and replaces it on
every change; there is no reason to edit it by hand, and doing so would be undone on the next
save. Nothing collected is stored by Krill — it goes straight to your own receiver.

## 2. Requirements

- **A Prometheus-compatible remote-write receiver** for metrics: Prometheus itself started with
  `--web.enable-remote-write-receiver`, Grafana Mimir, VictoriaMetrics, or Grafana Cloud's
  metrics endpoint.
- **A Loki push endpoint** for logs — self-hosted Loki or Grafana Cloud's logs endpoint.
- **Network access from every node**, not from the control plane: each node's agent connects to
  the receivers directly, so firewalls and DNS need to allow the connection from wherever your
  nodes actually are, cluster workers included.
- **The `grafana/alloy` image reachable from Docker Hub**, on every node. Krill pins it by
  digest (`grafana/alloy:v1.19.2@sha256:b8ec653c44235fbe910879145dac3597d66b0aaecf60bcbbe82580767771a839`),
  so if a node has no Docker Hub access, pre-pull that exact reference there before turning the
  agent on:

  ```sh
  docker pull grafana/alloy:v1.19.2@sha256:b8ec653c44235fbe910879145dac3597d66b0aaecf60bcbbe82580767771a839
  ```

  Do this on **every** node. Krill waits for the agent to be running only on the nodes Swarm can
  currently schedule a task on — a node that is down, drained or paused (see **Nodes**) is not
  part of that wait, so it does not hold up the rest of the cluster, but it also does not get the
  new configuration until it reconnects. If a node Swarm still considers healthy is nonetheless
  stuck (an image pull that never finishes, a manager-to-worker connection that drops mid-update),
  the whole attempt fails after a few minutes, same as before (see
  [Troubleshooting](#7-turning-it-off--troubleshooting)). Either way, once a pass does converge the
  page names any node still running the PREVIOUS configuration instead of quietly counting it as
  covered — see **Node coverage** below.

## 3. Turning it on

Open **Settings → Observability**. The page is instance-admin only, like Nodes and cluster
Monitoring.

For each of the two pipelines, fill in the **full push URL** — not just a host:

- **Metrics**: e.g. `https://mimir.example.com/api/v1/push` or, for Prometheus itself,
  `.../api/v1/write`.
- **Logs**: e.g. `https://loki.example.com/loki/api/v1/push`.

An empty URL turns that pipeline off; you can run metrics-only, logs-only, or both. User and
password are optional (basic auth); leaving the password field empty on an edit keeps the one
already stored — but only while the address still points at the same server (scheme, host and
port; a different path is fine). If you change the server, enter the password again: Krill
refuses to send a stored password to a host it wasn't entered for. To remove a stored password,
clear the user field and save.

Blanking a target's URL field alone does **not** clear it: Krill refuses the save and asks you to
tick **Remove these settings** next to that target first. This is deliberate — an address field
left empty by mistake (a browser that didn't restore it, a save that landed before you finished
editing) must not silently drop a working configuration.

**Check** sends one throwaway sample to each configured address before you commit to turning
the agent on: a metrics sample `krill_observability_check{krill_instance="..."}` over Remote
Write 2.0, and a Loki line under `{krill_check="observability"}`. Both stay in your storage —
Check doesn't undo the write. Each check opens one short-lived connection and reads at most a
bounded slice of the response; anything the receiver echoes back (including credentials it
happens to reflect in an error body) is redacted before it reaches the flash message. A failed
check **never blocks** turning the agent on — it's a diagnostic, not a gate.

| Result | Meaning |
|--------|---------|
| accepted | The receiver took the write. |
| receiver is Remote Write 1.0-only (HTTP 415) | Fine — the agent itself sends Remote Write 1.0 (Alloy's default `protobuf_message`); only the Check tool probes with 2.0 to also get write statistics back. |
| 2xx but unconfirmed | The receiver answered success but didn't return Remote Write 2.0's write-stat headers — it most likely only speaks 1.0, which again is what the agent sends. Treated as OK. |
| wrong user or password (401/403) | Credentials don't match what the receiver expects. |
| no receiver at this address (404) | Check the path — usually `/api/v1/push` or `/api/v1/write` for metrics, `/loki/api/v1/push` for logs. |
| private address | Krill doesn't probe private, loopback or link-local addresses unless `KRILL_ALLOW_PRIVATE_EGRESS=true` — but the agent runs on the node itself and can still send there regardless of this check. |
| unexpected answer / could not reach the receiver | Anything else — an HTTP status Krill didn't expect, a timeout, TLS failure, DNS failure, connection refused. |

Once saved and turned on, the page shows how many of the cluster's nodes are running the agent
and any error from the last reconcile pass.

**Node coverage.** A successful pass can still leave a node behind: Swarm's own node count for a
global service only reflects the nodes it could actually schedule a fresh task on, so a node that
was unreachable for this whole pass never shows up as missing from Swarm's side — it just quietly
keeps running its previous task. Krill still expects a container on any node that is not
**drained** — a node that is merely down, paused or otherwise unresponsive keeps whatever
container Swarm last placed there, so it is exactly the case this check exists to catch; only
draining a node actually removes its task, which is why a drained node is never named. When a
still-expected node lags, the page adds a line naming it on the **previous** settings (not merely
"unreachable" — after rotating a push credential this is the difference between "done" and "half
the cluster is still sending the old password"). This is not an error: the agent is deployed, and
Swarm places the task itself once the node reconnects (a few tens of seconds on a live cluster).
The check only runs right after Krill actually redeploys the agent — a pass that finds nothing
changed reports nothing new here (the existing "Agents running: X of Y" line already covers a node
that isn't running at all). The note is only as fresh as the last such pass — it clears the next
time one runs: saving settings, turning the agent on or off, renaming or removing a node on the
**Nodes** page, or restarting Krill. It does not refresh on its own between those (adding a node
does not by itself trigger a pass either — a freshly joined node has no label yet, so nothing about
the agent's configuration changes until it is named). The Observability page itself does not
auto-refresh either — reload it after triggering a pass to see the updated note once that pass has
actually finished.

## 4. Labels

| Label | Source | Applies to |
|-------|--------|------------|
| `krill_node` | The node's display name from **Nodes** (the same label the Nodes page, Topology and the app page show) — the raw Swarm hostname for a node that hasn't been given a name | Host metrics and every log line |
| `krill_org` | Organization name | App log lines |
| `krill_org_id` | Organization id | App log lines |
| `krill_project` | Project name | App log lines |
| `krill_env` | Environment name | App log lines |
| `krill_app` | App name | App log lines |
| `krill_app_id` | App id | App log lines |
| `krill_service` | Swarm's `com.docker.swarm.service.name` container label — e.g. `krill-<appID>` for an app | Log lines of every Swarm task: apps, database servers, Traefik, the agent itself |

Names can be renamed; ids can't, so a query keyed on an id keeps returning the same series
across a rename. The `_id` labels exist for that reason — prefer them for anything long-lived
like an alert or a saved dashboard.

`krill_node` used to always be the node's raw Swarm hostname (e.g.
`node-1.example.com`). It now follows the same name you give a node on the **Nodes**
page (Nodes → set a node's name), for both the manager and workers. **A node with no name set
keeps showing its raw hostname** — this is not a fallback for an edge case, it's what every node
on an install that has never named one looks like: upgrading to this feature changes nothing by
itself, silently. Give a node a name and the change takes effect on the agent's next reconcile
pass (a few seconds after saving); renaming or clearing a name does the same. A name is free text
(spaces and non-Latin text are fine) except for a quote, a backslash or a control character,
which the Nodes page itself refuses to save — but if you edit `node_labels` directly, or a name
saved before this restriction existed doesn't meet it, that node is simply left out of the
mapping (with a warning in the log), not allowed to break the agent's configuration. **If you
have saved queries or dashboards keyed on the old raw-hostname value** (from before this change,
or from running your own Alloy manually), either give the node the same value as a name on the
Nodes page, or update the queries to whatever name you choose — the label key is unchanged, only
its value is.

**An app's existing log lines get these labels only after its next deploy** — they come from
container labels Krill sets when it creates or updates the service, so a container already
running when you turn the agent on (or that predates this feature) won't carry them until it's
redeployed.

Example queries once data is flowing:

- Host memory available on one node: `node_memory_MemAvailable_bytes{krill_node="worker-1"}`
- All log lines from one app, any node it happens to run on: `{krill_app="web"}`

## 5. Cost

The agent's Swarm service is limited to **256 MiB of memory and half a CPU core, per node**.
Alloy sets its own `GOMEMLIMIT` from that cgroup limit — Krill does not pass one. Measured on a
live two-node cluster (2026-09-18, both pipelines on, real app and database traffic) across five
container generations over nine hours, the agent's actual working set — `anon` in
`/sys/fs/cgroup/memory.stat`, i.e. memory the kernel cannot reclaim without swapping — settled at
**51–59 MiB** on both nodes; a clean data volume and one that already held preserved read
positions from a previous generation both reach that same plateau in 3–4 minutes. What `docker
stats` and `memory.current` report is a different, larger number: both count the page cache
alongside that working set, and the cache does not shrink on its own while nothing on the node is
under memory pressure — one observation on a live node saw `docker stats` reach **237 MiB**; its
composition was not measured (the breakdown above was taken later, on a container then at 65 MiB),
and the figure was not reproduced. The kernel evicts page cache under real memory pressure well
before the OOM killer would ever run, so a number like that is headroom, not necessarily a leak —
but if you are budgeting memory for a small server, watch `anon`, not `docker stats`. Either way,
half a CPU core and up to 256 MiB per node is still a real share of the box — decide before turning
it on, especially on a single 2 vCPU / 2 GB node.

## 6. Security

- The agent needs the Docker socket to discover containers and read their logs — on Docker,
  socket access is effectively root on that node. Because of that:
  - it is **not** attached to any organization's overlay network — only the base network
    (`KRILL_NETWORK`) Krill uses for its own egress. Until the
    [per-organization network migration](../architecture.md) has finished, tenant services not
    yet moved still share that base network with the agent; since the agent listens on nothing
    reachable, that exposes nothing;
  - it **publishes no ports**; its own UI binds to `127.0.0.1:12345` inside the container,
    unreachable from anywhere else, including other nodes.
- The image is pinned by digest, not just by tag, so what runs is exactly what was reviewed.
- Passwords for the two push targets are Swarm secrets, mounted read-only at
  `/run/secrets/krill_metrics_password` / `/run/secrets/krill_logs_password`; the generated
  Alloy configuration references only that path, never the password itself. Krill encrypts
  them at rest (`KRILL_SECRET_KEY`) the same way it does every other stored credential.
- The settings page never renders or logs a password, stored or submitted.

## 7. Turning it off / troubleshooting

Turning the agent off removes the `krill-alloy-node` service from every node. What stays
behind:

- **Data already pushed** to your Prometheus-compatible storage and Loki — Krill doesn't own it
  and doesn't delete it.
- **Each node's local WAL volume** (`krill-alloy-node-data`) — Alloy's on-disk write-ahead
  buffer. Removing the service doesn't remove the volume; nothing in Krill does. Turning the
  agent back on reuses whatever is already on disk, per node.

Turning it back on redeploys the service from scratch with whatever settings are currently
saved.

**After rolling back to a Krill release without this feature** (a failed self-update, or a
manual rollback), nothing manages the agent any more and it keeps running. Remove it by hand:

```sh
docker service rm krill-alloy-node
docker config rm $(docker config ls -q --filter label=krill.observability=node-agent)
docker secret rm $(docker secret ls -q --filter label=krill.observability=node-agent)
```

**If a node's agent won't come up:**

```sh
docker service ps krill-alloy-node
```

shows the per-node task state — a node that can't pull the pinned image, or is unreachable,
shows as `Pending` or keeps restarting there while the rest of the cluster is fine.

**If you already run your own Alloy (or another agent) on the nodes**, remove it before turning
this one on. Two agents scraping the same host and tailing the same containers means duplicate
series and duplicate log lines in your storage, not a conflict Krill can detect.

## Application metrics

With a metrics destination enabled, Krill also runs `krill-alloy-apps`: one stop-first task on a manager, attached to every organization network, with its own persistent WAL volume. Its configured limits are 256 MiB and 0.25 CPU, in addition to the node agents. It has no host filesystem or Docker socket mounts, and its API listens only on container loopback.

Organization administrators configure endpoints and tokens on each application's **Metrics** tab. See [Application metrics](metrics.md) for setup, authentication, public-path protection, status, and failure behavior. The Settings page reports the app collector separately from the node agents. Removing the metrics destination removes the app collector; log-only node collection continues. Disabling observability removes both services and retains their data volumes.
