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

  Do this on **every** node — Krill waits for the agent to be running on **all** of them before
  it reports success, so even one node stuck pulling the image fails the whole attempt after a
  few minutes (see [Troubleshooting](#7-turning-it-off--troubleshooting)).

## 3. Turning it on

Open **Settings → Observability**. The page is instance-admin only, like Nodes and cluster
Monitoring.

For each of the two pipelines, fill in the **full push URL** — not just a host:

- **Metrics**: e.g. `https://mimir.example.com/api/v1/push` or, for Prometheus itself,
  `.../api/v1/write`.
- **Logs**: e.g. `https://loki.example.com/loki/api/v1/push`.

An empty URL turns that pipeline off; you can run metrics-only, logs-only, or both. User and
password are optional (basic auth); leaving the password field empty on an edit keeps the one
already stored.

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

## 4. Labels

| Label | Source | Applies to |
|-------|--------|------------|
| `krill_node` | The node's hostname | Host metrics and every log line |
| `krill_org` | Organization name | App log lines |
| `krill_org_id` | Organization id | App log lines |
| `krill_project` | Project name | App log lines |
| `krill_env` | Environment name | App log lines |
| `krill_app` | App name | App log lines |
| `krill_app_id` | App id | App log lines |
| `krill_service` | The app's Swarm service name (`krill-<appID>`) | App log lines |

Names can be renamed; ids can't, so a query keyed on an id keeps returning the same series
across a rename. The `_id` labels exist for that reason — prefer them for anything long-lived
like an alert or a saved dashboard.

**An app's existing log lines get these labels only after its next deploy** — they come from
container labels Krill sets when it creates or updates the service, so a container already
running when you turn the agent on (or that predates this feature) won't carry them until it's
redeployed.

Example queries once data is flowing:

- Host memory available on one node: `node_memory_MemAvailable_bytes{krill_node="worker-1"}`
- All log lines from one app, any node it happens to run on: `{krill_app="web"}`

## 5. Cost

The agent's Swarm service is limited to **256 MiB of memory and half a CPU core, per node**.
Alloy sets its own `GOMEMLIMIT` from that cgroup limit — Krill does not pass one. Measured
idle usage was about **50 MiB** on a Colima development VM (2026-09-16); expect the live
two-node acceptance run to refine this number under real load. On a small server, half a CPU
core and up to 256 MiB per node is still a real share of the box — decide before turning it on,
especially on a single 2 vCPU / 2 GB node.

## 6. Security

- The agent needs the Docker socket to discover containers and read their logs — on Docker,
  socket access is effectively root on that node. Because of that:
  - it is **not** attached to any organization's overlay network — only the base network Krill
    uses for its own egress;
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

**If a node's agent won't come up:**

```sh
docker service ps krill-alloy-node
```

shows the per-node task state — a node that can't pull the pinned image, or is unreachable,
shows as `Pending` or keeps restarting there while the rest of the cluster is fine.

**If you already run your own Alloy (or another agent) on the nodes**, remove it before turning
this one on. Two agents scraping the same host and tailing the same containers means duplicate
series and duplicate log lines in your storage, not a conflict Krill can detect.
