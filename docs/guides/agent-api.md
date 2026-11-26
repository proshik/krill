# Agent and CI API (REST + MCP)

Krill exposes twenty operations for AI agents and CI pipelines over two surfaces backed by the
same code: **REST/JSON at `/api/v1`** for scripts, and an **MCP server (streamable HTTP) at
`/mcp`** for agents. Both use the same organization-scoped bearer tokens and the same tenancy
checks.

Sixteen **read and operate what already exists** — status (with Swarm tasks, container health and
memory/CPU), logs (with a time window, a substring filter and waiting for a line), saved settings,
backups, a DNS and certificate check of an app's domains, deploy / rebuild / reload / stop,
single-variable env edits and starting a configured backup. Four more **create** what moving an
app onto Krill needs — a logical database, an image app, a domain, a database link — and only for
a token issued at the **provision** level. There are no destructive operations: nothing deletes
or changes an existing organization, project, environment, app, database, domain or volume, and
there is no shell into a container. The blast radius is bounded by the surface itself — a list of
twenty operations can be checked by eye.

## 1. Issue a token

Open **Settings → API tokens** and create one: a name, a level (**read**, **write** or
**provision**) and an expiry (never, 30 or 90 days). Write includes read; provision includes
write. Give CI pipelines write tokens: only provision tokens can create resources. Creating tokens is admin-only; any member can list and revoke
their own.

- The token (`krill_pat_…`) is shown **once**. Krill stores only its SHA-256 hash; a lost token
  can't be recovered — revoke it and issue a new one.
- Rights are checked **on every request**: the token's level, capped by its owner's *current*
  role in the organization. Demote the owner to `member` or remove them, and their write token
  stops writing at once, with no separate revocation.
- A token belongs to one organization. An app in another organization answers *not found*,
  never *forbidden*.

## 2. Connect an agent (MCP)

```sh
claude mcp add --transport http krill https://krill.example.com/mcp \
  --header "Authorization: Bearer krill_pat_..."
```

| Level | Tools |
|-------|-------|
| read | `krill_whoami`, `krill_list_apps`, `krill_app_status`, `krill_app_logs`, `krill_deployments`, `krill_deployment_status`, `krill_list_env`, `krill_app_config`, `krill_list_backups`, `krill_domain_status` |
| write | `krill_deploy`, `krill_rebuild`, `krill_reload`, `krill_stop`, `krill_set_env`, `krill_backup_now` |
| provision | `krill_create_database`, `krill_create_app`, `krill_add_domain`, `krill_link_database` |

Apps are addressed by a `project/environment/app` path or a numeric id; `krill_list_apps`
returns both.

[`skills/krill-deploy`](../../skills/krill-deploy/SKILL.md) teaches an agent the
deploy-and-verify and diagnose-a-failure runbooks: wait on the server instead of polling, don't
retry a failed deploy, never invent a variable's value, ask before touching production. It is meant to
be used from your application's repository, so install it globally:

```sh
cp -r skills/krill-deploy ~/.claude/skills/
```

## 3. Call it from CI (REST)

The token goes in the `Authorization` header and **only** there — `?token=` is not accepted,
because query strings end up in proxy logs and browser history.

```sh
# Deploy an image app at a new tag, then check the deployment
DEPLOY=$(curl -sS -X POST \
  -H "Authorization: Bearer $KRILL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tag":"v1.2.3"}' \
  https://krill.example.com/api/v1/apps/acme/production/bot/deploy)

ID=$(echo "$DEPLOY" | jq -r .deployment_id)
# Answers once the deployment is done or failed, or after 120 s
curl -sS -H "Authorization: Bearer $KRILL_TOKEN" \
  "https://krill.example.com/api/v1/deployments/$ID?wait=120"
```

| Route | Level | Notes |
|-------|-------|-------|
| `GET /api/v1/whoami` | read | User, organization, token level, live role, `can_write`, and `instance` — this installation's public URL and version, to tell installations apart (app ids repeat across them). |
| `GET /api/v1/apps` | read | Every app in the organization: path, id, status, source type, image and tag, domains. |
| `GET /api/v1/apps/{app}` | read | One app: status, `running/desired` replicas, node, domains, last deployment; and, when Swarm answers, its five newest `tasks` (node, state, desired state, error, exit code, times — what `docker service ps` shows), `failed_since_deploy` (tasks created since the last deployment started that crashed, were refused or exited; a lower bound, as Swarm keeps about five tasks per replica), `health` of each container meant to run (status, failing streak, last probe's exit code and time — never its output) and `usage` (memory read live; CPU from the monitoring sampler when it can only be that container's). A part that cannot be read is left out. |
| `GET /api/v1/apps/{app}/logs` | read | Runtime log tail: the most recent lines across all of the app's tasks (current and recently stopped), merged by time, oldest first. `?tail=` (default 200, max 1000), `?level=` (`trace` … `fatal`) — drops only lines whose detected level is below it; a line with no detected level (`"lvl": ""`) is always kept. The level comes from a `level=` field or a severity word (`INFO`, `WARN`, `ERROR`, …) near the start of the line, so most unstructured output has none and passes the filter. `?since=` and `?until=` take an RFC3339 time or a duration ago (`10m`, `2h`); `until` needs `since`. `?contains=` keeps lines whose message contains the text, ignoring case (it searches up to each task's last 10 000 lines of the window, or 50 000 lines in all for a window with an end; a search that may have missed lines ends with a "log read incomplete" line). A line over 16 KB is cut, saying so. `?wait_for=` waits instead — see below. |
| `GET /api/v1/apps/{app}/deployments` | read | History, newest first. `?limit=` (default 20, max 50). |
| `GET /api/v1/deployments/{id}` | read | One deployment and the last ~8 KB of its build log. `?wait=` (seconds, max 120) waits while it is still running and answers once it is done or failed, or when the time is up. `"settling": true` means the spec is applied and Swarm is still converging — still `running`, not done. |
| `GET /api/v1/apps/{app}/config` | read | Saved settings, never a secret: source (image and tag, or git repository — scheme, host and path only, left out when it cannot be read safely —, branch, Dockerfile path, build args and secrets by name), resources (replicas, memory/CPU limits and whether they are the app's own or the instance default, restart policy, placement), healthcheck timing (not its command), ports, volumes, domains (paths, exposed, HTTPS, redirect, which protection kinds), links by variable, and `pending` — the settings sections saved but not applied yet. |
| `GET /api/v1/apps/{app}/domain-status` | read | For each domain: what DNS resolves it to from the control plane, whether that is one of the addresses Krill knows for its gateway — `KRILL_ADVERTISE_ADDR` and the node its gateway task runs on (`points_at_gateway`; `gateway_known` is false when Krill knows none, and the addresses themselves are not listed — they are the nodes'). Behind NAT or a private network, the public address can be none of them, and for an HTTPS domain the certificate the gateway serves for it — issuer, validity, `days_left`, `trusted`, `default_cert` (Traefik's own self-signed certificate: none issued yet). |
| `GET /api/v1/backups` | read | Every configured backup of the organization — `database` (pg_dump), `volume` and `bucket` — with target, destination, schedule, retention, `enabled`, `running`, last run (`last_run_at`, to the microsecond; `last_status`; `last_error` — a summary Krill writes of what went wrong, never the stored text, which can carry a credential: the full error is on the backup's page in the UI) and `last_success_at`. `?app=` (its volumes and the databases and buckets it is linked to) or `?environment=project/env`. |
| `GET /api/v1/apps/{app}/env` | read | Variable **names** and their source (`literal` / `db-link` / `bucket-link`) — never values. |
| `POST /api/v1/apps/{app}/deploy` | write | Optional body `{"tag":"v1.2.3"}` for image apps — a bare tag, not a full image reference. Returns `{deployment_id, status}`. |
| `POST /api/v1/apps/{app}/rebuild` | write | Build without cache; Dockerfile apps only. Returns `{deployment_id, status}`. |
| `POST /api/v1/apps/{app}/reload` | write | Restart the tasks in place — same image, no build, no pull (`{"action":"restart","status":"ok"}`). A stopped app is deployed instead: `{"action":"deploy","status":"running","deployment_id":…}`. |
| `POST /api/v1/apps/{app}/stop` | write | Scale to zero; a deploy brings it back. A deploy that has not applied its spec yet is cancelled — but a tag it carried is already the app's tag, so a later deploy without `tag` ships that one; pass the previous tag to go back. If Stop answers with an error, the service may already be scaled to zero while the cancelled deploy still reads `running` (its outcome could not be written); call Stop again before a new deploy — it retries the write. |
| `POST /api/v1/apps/{app}/env` | write | `{"key":"K","value":"V"}` or `{"key":"K","remove":true}` — one line, the rest untouched. The value must be a single line (base64-encode a PEM key or JSON). |
| `POST /api/v1/backups/{kind}/{id}/run` | write | Start a configured backup now, like **Back up now** in the UI. It applies the backup's retention like a scheduled run, so it can delete the oldest stored copies (with retention 1, the only older one). Answers `{"status":"started","started_at":…}`; follow it in `GET /api/v1/backups` until `running` is false — its result is the one whose `last_run_at` equals `started_at` (a later one is a later run's; an earlier one means this run recorded nothing). `409` when a backup or restore of it is already running. |
| `POST /api/v1/environments/{project}/{env}/databases` | provision | `{"name":"app","server":"main"}` (+ optional `db_name`, `username`) — a logical database and its owner user inside an existing Postgres server, which must be running. The password is never returned. |
| `POST /api/v1/environments/{project}/{env}/apps` | provision | `{"name":"bot","image":"ghcr.io/acme/bot","tag":"v1","port":8080}` (+ optional `registry`, `domain`) — an image app, **not deployed**. `domain` is `{"host","tls","exposed","paths"}`; without it the app gets `<name>.<base domain>`, internal and without HTTPS. |
| `POST /api/v1/apps/{app}/domains` | provision | `{"host":"bot.example.com","tls":true,"exposed":true,"paths":["/webhook"]}` — another domain of the app; routing applies at once to a deployed app. A host other apps already use has one HTTPS setting for all of them: omit `tls` to take it, or pass the same value. |
| `POST /api/v1/apps/{app}/db-links` | provision | `{"var":"DATABASE_URL","database":"app"}` (a logical database of the app's environment) or `{"var":"REDIS_URL","server":"cache"}` (a Redis-compatible server); optional `field` — default `url`; an S3 server needs one of `endpoint`, `access_key`, `secret_key`, `region` (`{"var":"S3_ENDPOINT","server":"files","field":"endpoint"}`) — and, for Postgres, `scheme` (`postgres` or `postgresql`). Applied on the next deploy. |

A database server or registry is named by its name or id; a reference that is one's name and
another's id is refused as ambiguous, and so is an environment path when two projects share a
name. A new database is refused (`409`) when the server already has a database or role of that
name that Krill does not manage — it is never touched.

The four creating routes are **idempotent** on a natural key — the name in the environment, the
host on the app, the variable on the app. Repeating a request that already took effect returns
the existing resource with `"created": false`; one that names an existing resource with other
settings answers `409 conflict` and changes nothing. Changing or deleting a resource is done in
the web UI.

Deploys are **asynchronous**: `deploy` and `rebuild` return a `deployment_id` at once, and the
caller waits with `GET /api/v1/deployments/{id}?wait=120` (repeat while it still says `running`)
until `running` becomes `done` or `error`. `done`
means Swarm finished the rolling update with the new tasks running. A deployment is `running`
while it waits in the queue and builds, then for the convergence wait —
`KRILL_CONVERGE_TIMEOUT`, or a healthcheck's start period plus a minute when that is longer —
and, if the service is still starting then (an image still pulling), for up to 20 more
minutes before it becomes `done` or `error`; keep polling. While it runs, `log_tail` is empty — the build log is saved
when the deployment ends, and the live log is on the app's Deploys tab. A deploy that never
converges ends `error` with `⚠ service still starting` and `❌ service did not converge` in its
log. While it is in that last phase the answer says `"settling": true`. An env
edit changes the app's configuration but restarts nothing; it takes effect on the next deploy.

### Waiting for a log line

`GET /api/v1/apps/{app}/logs?wait_for=listening&timeout=90` watches the app's log for a **new**
line containing `listening` (ignoring case), at most `timeout` seconds (default 60, max 120). It
looks only at lines written after the call opened the live stream, whatever their timestamps (a
worker's clock may lag); it does not search what was logged before. The answer is an object:
`{"found", "stopped_by", "match", "lines", "following_since", "incomplete"}` — `stopped_by` is
`match`, `timeout`, `ended` (the service went away), `error` (the live read broke),
`shutdown` (Krill is restarting) or `cancelled`; `lines` are the `tail` lines before the match
(default 20), the match and a few after it; `following_since` is when the live stream was open —
lines written before it are not covered; `incomplete` says the live read broke, so a missing match
is no proof the line never appeared. `wait_for` cannot be combined with `since`, `until`,
`contains` or `level`. An installation runs a few log waits at once; a further one answers
`503 unavailable` — retry shortly.

To wait for a line after a deploy without a gap between calls:

1. Read with `contains=listening&since=<the deployment's started_at>`. If the line is there, done.
2. Otherwise wait: `wait_for=listening&timeout=90`.
3. On a timeout, read with `contains` and the same `since` once more. It covers the moments
   between steps 1 and 2 that neither call saw.

## Notes

- `KRILL_AGENT_API_ENABLED` (default `true`) switches **both** surfaces; with `false`, `/api/v1`
  and `/mcp` answer 404.
- **60 requests per minute per token**, then `429` with `Retry-After`. MCP spends the budget
  faster than REST: a session costs an `initialize` and a `tools/list` before the first call.
  Give each machine or agent its own token. A call that waits (`wait`, `wait_for`) counts once,
  however long it waits, and ends early when Krill shuts down.
- `401` means the token was rejected; `503` with `code: "unavailable"` means it couldn't be
  checked (the database is down) — retry that one rather than reissuing the token.
- A deploy, rebuild or reload that is not accepted creates no deployment and changes nothing:
  `409 conflict` when this app already has a deployment in progress (poll it), `503
  unavailable` with `Retry-After` when the queue is full, the organization has
  `KRILL_MAX_BUILDS_PER_ORG` deployments in progress, or Krill is shutting down. An accepted one
  always returns a `deployment_id`.
- Reading env values is deliberately impossible: everything an agent reads reaches its model
  provider. Writing a value is allowed.
- Tokens cross the network in clear text over plain HTTP. Krill logs a warning at startup when
  the API is on and the public URL isn't `https://` — put Krill behind TLS.
- Every write is logged with the token id and user id — never the token or an env value.
- An app named like one of the route verbs (`logs`, `env`, `deployments`, `deploy`, `rebuild`,
  `reload`, `stop`) can only be addressed over REST by its numeric id; one named `domains` or
  `db-links` only for the provisioning POSTs. One named `config` or `domain-status` keeps its
  status route: those two words are verbs only after a full `project/environment/app` path or an id.
- Several installations: app ids repeat across them. Check `instance` in `whoami` before a write
  and address apps by path. `instance.url` tells installations apart only when each has its own
  `KRILL_PUBLIC_URL` or panel domain; two that both say `http://localhost` cannot be told apart
  this way.

## Webhooks

Separately from the API, each app can opt into **auto-deploy** in its **Settings** tab, **Auto-deploy**:

- **Dockerfile apps:** add the shown URL and secret as a GitHub webhook for `push` events. A push
  to the configured branch builds and deploys; the signature (`X-Hub-Signature-256`) is
  verified.
- **Image apps:** call the deploy hook from CI after `docker push`, with the secret as a bearer
  token and an optional `?tag=`.

When a deploy can't be queued — one is already running for that app, or the organization is at
`KRILL_MAX_BUILDS_PER_ORG` — both endpoints answer `503` with `Retry-After`. GitHub does not
retry failed deliveries; redeliver from the webhook's *Recent Deliveries* page.
