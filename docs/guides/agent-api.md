# Agent and CI API (REST + MCP)

Krill exposes twelve operations for AI agents and CI pipelines over two surfaces backed by the
same code: **REST/JSON at `/api/v1`** for scripts, and an **MCP server (streamable HTTP) at
`/mcp`** for agents. Both use the same organization-scoped bearer tokens and the same tenancy
checks.

They **operate apps that already exist** — status, logs, deploy / rebuild / reload / stop, and
single-variable env edits. There are no destructive operations: nothing creates or deletes an
organization, project, environment, app, database, domain or volume, and there is no shell
into a container. The blast radius is bounded by the surface itself — a list of twelve
operations can be checked by eye.

## 1. Issue a token

Open **Settings → API tokens** and create one: a name, a level (**read** or **write**) and an
expiry (never, 30 or 90 days). Creating tokens is admin-only; any member can list and revoke
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
| read | `krill_whoami`, `krill_list_apps`, `krill_app_status`, `krill_app_logs`, `krill_deployments`, `krill_deployment_status`, `krill_list_env` |
| write | `krill_deploy`, `krill_rebuild`, `krill_reload`, `krill_stop`, `krill_set_env` |

Apps are addressed by a `project/environment/app` path or a numeric id; `krill_list_apps`
returns both.

[`skills/krill-deploy`](../../skills/krill-deploy/SKILL.md) teaches an agent the
deploy-and-verify and diagnose-a-failure runbooks: poll on a widening interval, don't retry a
failed deploy, never invent a variable's value, ask before touching production. It is meant to
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
curl -sS -H "Authorization: Bearer $KRILL_TOKEN" \
  "https://krill.example.com/api/v1/deployments/$ID"
```

| Route | Level | Notes |
|-------|-------|-------|
| `GET /api/v1/whoami` | read | User, organization, token level, live role, `can_write`. |
| `GET /api/v1/apps` | read | Every app in the organization: path, id, status, source type, image and tag, domains. |
| `GET /api/v1/apps/{app}` | read | One app: status, `running/desired` replicas, node, domains, last deployment. |
| `GET /api/v1/apps/{app}/logs` | read | Runtime log tail: the most recent lines across all of the app's tasks (current and recently stopped), merged by time, oldest first. `?tail=` (default 200, max 1000), `?level=` (`trace` … `fatal`) — drops only lines whose detected level is below it; a line with no detected level (`"lvl": ""`) is always kept. The level comes from a `level=` field or a severity word (`INFO`, `WARN`, `ERROR`, …) near the start of the line, so most unstructured output has none and passes the filter. |
| `GET /api/v1/apps/{app}/deployments` | read | History, newest first. `?limit=` (default 20, max 50). |
| `GET /api/v1/deployments/{id}` | read | One deployment and the last ~8 KB of its build log. |
| `GET /api/v1/apps/{app}/env` | read | Variable **names** and their source (`literal` / `db-link`) — never values. |
| `POST /api/v1/apps/{app}/deploy` | write | Optional body `{"tag":"v1.2.3"}` for image apps — a bare tag, not a full image reference. Returns `{deployment_id, status}`. |
| `POST /api/v1/apps/{app}/rebuild` | write | Build without cache; Dockerfile apps only. Returns `{deployment_id, status}`. |
| `POST /api/v1/apps/{app}/reload` | write | Restart the tasks in place — same image, no build, no pull (`{"action":"restart","status":"ok"}`). A stopped app is deployed instead: `{"action":"deploy","status":"running","deployment_id":…}`. |
| `POST /api/v1/apps/{app}/stop` | write | Scale to zero; a deploy brings it back. |
| `POST /api/v1/apps/{app}/env` | write | `{"key":"K","value":"V"}` or `{"key":"K","remove":true}` — one line, the rest untouched. The value must be a single line (base64-encode a PEM key or JSON). |

Deploys are **asynchronous**: `deploy` and `rebuild` return a `deployment_id` at once, and the
caller polls `GET /api/v1/deployments/{id}` until `running` becomes `done` or `error`. An env
edit changes the app's configuration but restarts nothing; it takes effect on the next deploy.

## Notes

- `KRILL_AGENT_API_ENABLED` (default `true`) switches **both** surfaces; with `false`, `/api/v1`
  and `/mcp` answer 404.
- **60 requests per minute per token**, then `429` with `Retry-After`. MCP spends the budget
  faster than REST: a session costs an `initialize` and a `tools/list` before the first call.
  Give each machine or agent its own token.
- `401` means the token was rejected; `503` with `code: "unavailable"` means it couldn't be
  checked (the database is down) — retry that one rather than reissuing the token.
- Reading env values is deliberately impossible: everything an agent reads reaches its model
  provider. Writing a value is allowed.
- Tokens cross the network in clear text over plain HTTP. Krill logs a warning at startup when
  the API is on and the public URL isn't `https://` — put Krill behind TLS.
- Every write is logged with the token id and user id — never the token or an env value.
- An app named like one of the route verbs (`logs`, `env`, `deployments`, `deploy`, `rebuild`,
  `reload`, `stop`) can only be addressed over REST by its numeric id.

## Webhooks

Separately from the API, each app can opt into **auto-deploy** on its **General** tab:

- **Dockerfile apps:** add the shown URL and secret as a GitHub webhook for `push` events. A push
  to the configured branch builds and deploys; the signature (`X-Hub-Signature-256`) is
  verified.
- **Image apps:** call the deploy hook from CI after `docker push`, with the secret as a bearer
  token and an optional `?tag=`.

When a deploy can't be queued — one is already running for that app, or the organization is at
`KRILL_MAX_BUILDS_PER_ORG` — both endpoints answer `503` with `Retry-After`. GitHub does not
retry failed deliveries; redeliver from the webhook's *Recent Deliveries* page.
