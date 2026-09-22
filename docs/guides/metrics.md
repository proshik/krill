# Application metrics

Krill runs `krill-alloy-apps`, a single Grafana Alloy collector on a Swarm manager, attached to every organization's overlay network. It discovers each application's replicas through Swarm DNS and scrapes their Prometheus endpoints every 30 seconds. It writes to the metrics destination configured in **Settings → Observability**.

Each series carries `job`, `instance`, `krill_org`, `krill_org_id`, `krill_project`, `krill_env`, `krill_app`, `krill_app_id`, and `krill_node`. Node names match the labels on the Nodes page, falling back to the Swarm hostname. Name labels can change; numeric organization and application IDs stay stable.

## Turn collection on

1. An instance administrator configures and enables a metrics destination in **Settings → Observability**.
2. An organization administrator opens the application's **Metrics** tab and selects **Collect metrics**. Krill creates an endpoint on the application's HTTP port at `/metrics` and generates a token.
3. Configure the application to expose Prometheus metrics, then **deploy it** so it receives the token.
4. Use **Refresh** on the Metrics tab to check collection. In Grafana, query `up{krill_app="your-app"}`.

Collection can be disabled without deleting its endpoints or token. Up to ten endpoints can be configured, each with a port, path, and optional job name. An empty job uses the application's name.

## Application contract

Serve Prometheus text format and accept `Authorization: Bearer <token>`. Krill supplies the token through `KRILL_METRICS_TOKEN`; the variable can be renamed on the Metrics tab. Krill always sends this header. An application may choose not to check it, but then hiding the path on the domain (below) is its only protection, and that is best effort: check the token wherever you can.

The variable name cannot conflict with the Environment tab or an injected database link. The token is encrypted using the existing `KRILL_SECRET_KEY` mechanism. Only organization administrators can reveal it; members can inspect endpoints and collection status.

### Examples

- **Gitea:** if its Prometheus endpoint is already enabled and configured, use its existing endpoint without changing application code. Ensure its configured authentication matches the Krill token, if authentication is enabled.
- **Keycloak:** enable metrics in Keycloak and add its management endpoint, normally port `9000`, path `/metrics`. Exposing this port publicly is unnecessary.
- **A relay service:** configure `METRICS_ADDR=:27015`, add `27015 /metrics` with job `relay`, and rename the token variable to `METRICS_TOKEN` before deploying.
- **Go with client_golang:** protect the handler with the injected token. An empty token must reject requests:

```go
metrics := promhttp.Handler()
http.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    token := os.Getenv("KRILL_METRICS_TOKEN")
    expected := "Bearer " + token
    if token == "" || subtle.ConstantTimeCompare(
        []byte(r.Header.Get("Authorization")), []byte(expected),
    ) != 1 {
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return
    }
    metrics.ServeHTTP(w, r)
}))
```

The example uses `crypto/subtle`, `net/http`, `os`, and `github.com/prometheus/client_golang/prometheus/promhttp`.

## Why the public path returns 404

While collection is enabled, endpoints on the application's HTTP port are excluded from every domain router, including HTTP redirects and HTTPS routes. The match ignores case and covers the path's descendants and `;` path parameters: for `/metrics`, the domain also refuses `/METRICS`, `/metrics/…` and `/metrics;…`, spellings that some backends (ASP.NET, servlet containers) still route to the same handler. Other paths, such as `/metricsfoo`, continue to use the domain's normal exposure and access rules. The rule can only anticipate how common backends normalize paths, so treat it as a second line behind the token check, not a replacement for it.

An endpoint cannot use a container port that is published over TCP directly to the host. Conversely, adding such a published port is refused while an endpoint references it, even when collection is disabled. An endpoint on a separate internal port is accessible to the collector through the overlay network.

## Read collection status

The Metrics tab reads the running collector's own API through Docker exec. It reports the first condition preventing collection:

- Observability is off or no metrics destination is configured.
- The collector is unavailable, stopped, or has not fetched its rules since Krill started.
- The last successful rules fetch is older than two minutes.
- The application is not deployed, is stopped, or needs redeployment to receive the current token or variable name.
- The collector rejected new rules, or its API cannot be read.
- An endpoint has not appeared in the module yet, DNS has no replicas, or replicas do not yet have a known node.

Otherwise, the table shows each target's address, node, `up`/`down`, last scrape time, and last error. A `401` usually means the application and collector disagree about the token. A token fingerprint is stored on the application's **container specification**, so domain edits cannot erase the evidence of which token was deployed.

## Changes and failures

Endpoint and label changes normally reach the collector within 30 seconds. A new replica also waits for discovery and the next node-address mapping. Token rotation and variable-name changes reach the application only on its next deployment; authentication can fail until then.

If Krill becomes unavailable, an already running collector keeps its last rules and continues scraping. If the collector restarts while Krill is unavailable, its initial module load fails and Swarm retries until Krill returns. The node collector is independent.

Creating an organization adds another overlay network to the collector. This requires a stop-first rollout and briefly interrupts application collection; network-triggered passes are limited to one per minute. The collector uses its own persistent WAL volume, `krill-alloy-apps-data`, which is retained when collection is disabled. It has a 256 MiB memory limit and a 0.25 CPU limit. It is a limit, not a reservation: the collector uses only what the series it holds need.

Memory grows with the total number of series across all collected applications. Measured on Alloy v1.19.2 by anonymous memory (`anon` in the container's `memory.stat`, not `docker stats`), three minutes per step, one target per application:

| Series in total | Anonymous memory |
|---|---|
| none | 37 MiB |
| 1,000 | 44 MiB |
| 5,000 | 62 MiB |
| 10,000 | 80 MiB |
| 25,000 | 116 MiB |
| 50,000 | 157 MiB |

A typical service exposes a few hundred to a few thousand series; count yours with `count({krill_app="..."})` in Grafana. The limit leaves room for roughly 50,000 series with a margin. Beyond that, raise `AppsMemoryLimit`. An earlier 64 MiB limit, sized from four targets with one series each, would have run out at about 5,000 series.

Each endpoint is limited per scrape to 5,000 samples, a 5 MiB response body, 40 labels per series and 2,048 characters per label value. An endpoint over a limit fails on its own — the Metrics tab shows it `down` with the reason in its last error — and none of that scrape's samples are written; other endpoints and applications are unaffected.
