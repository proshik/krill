# krill-cli — build locally, deploy to Krill

`krill-cli` builds a container image **on your machine** and deploys it to an existing Krill
app. It is for when building in CI isn't an option — free build minutes run out — and a
2 vCPU / 2 GB server is not where you want to run `docker build` either.

It deploys apps that already exist; projects, environments and apps are created in the web UI.

## Install

Download the latest release tarball for your platform — `linux` or `darwin`, `amd64` or
`arm64`:

```sh
VERSION=$(curl -sSLo /dev/null -w '%{url_effective}' https://github.com/proshik/krill/releases/latest | sed 's#.*/##')
curl -sSL "https://github.com/proshik/krill/releases/download/$VERSION/krill-cli_${VERSION}_darwin_arm64.tar.gz" \
  | tar -xz && sudo mv krill-cli /usr/local/bin/
```

or build it with Go:

```sh
go install github.com/proshik/krill/cmd/krill-cli@latest
```

## Deploy

```sh
krill-cli login --server https://krill.example.com   # reads the API token from stdin
cd ~/code/my-bot
krill-cli init                                       # pick the app; writes krill.yaml
krill-cli deploy
```

```
$ krill-cli deploy

  app       acme/production/bot  (image)
  image     ghcr.io/acme/bot
  tag       main-a1b2c3d4e5f6
  platform  linux/amd64
  via       registry

✓ build    18.4s
✓ push     ghcr.io/acme/bot:main-a1b2c3d4e5f6   41.2s
✓ deploy   queued  #418
✓ rollout  1/1 running

https://bot.example.com
```

The token is an [API token](agent-api.md#1-issue-a-token) with write level.

## `krill.yaml`

Committed with your application. It holds **no secrets** — the token lives in
`~/.config/krill/config.json` — and an unknown key is a parse error, which keeps a `token:` from
quietly ending up in a commit.

```yaml
app: acme/production/bot
image:
  repository: ghcr.io/acme/bot    # must match the app's image in Krill
  platform: linux/amd64           # what the SERVER runs, not your laptop
build: { context: ., dockerfile: Dockerfile }
tag:   { strategy: git }          # git | timestamp
delivery: registry
```

**`platform` defaults to `linux/amd64`, not to your machine.** An image built for an
Apple-silicon laptop and deployed to an amd64 server loads fine and dies on start with
`exec format error`, visible only in the container log. If your server is arm64, say so here.

## How the image reaches the server

| `delivery` | What happens |
|------------|--------------|
| `registry` (default) | `docker push`, then Krill pulls. Only changed layers cross the network — usually a few MB per deploy. Needs a registry account; for a private repository, Krill also needs [its own pull credentials](registries.md#private-registries). |
| `upload` | **Planned, not implemented** — refused before anything is built. It would stream the image straight to Krill without a registry, at the cost of shipping the whole image on every deploy. |

## Commands

`deploy` (alias `up`) · `init` · `login` · `context [use|rm]` · `whoami` · `apps` · `status` ·
`config` · `domains` · `logs` · `deployments` · `deployment ID [--watch]` · `env [set|rm]` ·
`backups [run]` · `stop` · `reload` · `rebuild` · `version` · `completion`

Every command takes `-c/--context NAME` (which stored login to use) and `--json`. With `--json`,
the read commands (`whoami`, `apps`, `status`, `config`, `domains`, `logs`, `deployments`,
`deployment`, `env`, `backups`), `deployment ID --watch`, `rebuild` and `backups run` print only
the server's answer on stdout, as JSON, and messages go to stderr. `deploy`, `stop`, `reload`
and `env set|rm` print progress or a confirmation line whatever `--json` says — read their exit
code. An `APP` argument is a numeric id or a `project/environment/app` path and defaults to
`krill.yaml`'s `app`.

| Command | What it shows or does |
|---------|-----------------------|
| `whoami` | Organization, token level, the owner's live role, `can write`, and which installation answered — `instance` URL and version. App ids repeat across installations: check this before a write when you have several contexts. |
| `status [APP]` | Status, replicas, node, image, domains, last deployment, and when Swarm answers: the newest tasks (state, node, exit code, error — what `docker service ps` shows), tasks that ended since the last deploy, container health and memory/CPU. While the last deployment runs, it says whether it is applied and Swarm is still converging. |
| `config [APP]` | Saved settings without secrets — source, resources, placement, healthcheck timing, ports, volumes, domains, links — and `pending`: sections saved but not applied yet. |
| `domains [APP]` | Per domain: what DNS resolves it to, whether that is Krill's gateway, and the certificate the gateway serves (trusted, days left, or Traefik's default one: none issued yet). |
| `logs [APP]` | The runtime log, newest lines across all tasks. `--tail`, `--level`, a window `--since`/`--until` (RFC3339 or `10m`), `--contains TEXT` (ignores case). `--wait-for TEXT --timeout 90s` waits on the server for a **new** line instead (at most 2 minutes; not combinable with the window, `--contains` or `--level`). |
| `deployment ID [--watch]` | One deployment and its build log tail; `--watch` waits until it ends. |
| `backups [--app APP \| --env PROJECT/ENV]` | Configured backups: schedule, retention, running, last run, its status and error, last success. |
| `backups run KIND ID [--wait]` | Start a configured backup now (write level). It applies the backup's retention. `--wait` follows **this** run to its result. |

**Waiting happens on the server.** `deploy`, `rebuild --watch` and `deployment ID --watch` ask
Krill to hold each request while the deployment runs and to answer the moment it ends, so a
ten-minute build costs a handful of requests (each waits up to 2 minutes, never past
`--timeout`). Krill saves the build log when the deployment ends, so the log tail is printed
then; with `--json`, `deployment ID --watch` and `rebuild --watch` print no log and end with the
last deployment the server sent. Against a Krill too old to wait, the CLI notices the immediate
answers and falls back to its old poll schedule (1, 2, 3, then every 5–10 s), which stays inside
the token's rate limit. A `502`/`504` from a proxy in front of Krill counts as lost contact: it
is retried, and a watch that never gets an answer exits `4`, not `1`.

**An older Krill.** `whoami` shows no instance, and `config`, `domains`, `backups`,
`logs --since/--until/--contains` and `logs --wait-for` exit `2` saying the server needs an
update — they never print a plausible answer the server did not actually filter.

### Exit codes

| Code | Meaning |
|------|---------|
| `0` | ok; for `logs --wait-for`, the line was seen; for `backups run --wait`, this run succeeded |
| `1` | the deployment failed; `backups run --wait`: this run failed or recorded nothing; also an unexpected error |
| `2` | configuration or arguments — a bad flag, a missing or ill-formed app, a contradictory filter, or a Krill too old for the command. Nothing was attempted |
| `3` | another deploy was in flight (`--no-wait-for-lock`), or a backup of that one is already running |
| `4` | stopped waiting and the outcome is unknown: a watch timed out (or lost the server); `logs --wait-for` did not see the line (timeout, the log ended, or a broken live read — no proof it never came); `backups run --wait` timed out or a later run overwrote the result |
| `5` | deployed but not running |
| `6` | authentication — the token was rejected or lacks the level |

## Notes

- **Checks run before the build.** The token's level, the app's existence and source type, and
  whether `image.repository` matches what Krill pulls are verified first — a repository
  mismatch would otherwise produce a green deploy of the *old* image.
- **`krill-cli` and MCP complement each other.** MCP can't carry an image, so the build happens
  in the CLI. Everything the MCP tools read is also a `krill-cli` command, so an agent without
  MCP, or with several Krill installations, can work through `krill-cli -c CONTEXT --json …`
  (the [krill-deploy skill](../../skills/krill-deploy/SKILL.md) describes that path).
- **Use a separate token per machine.** The 60 requests/minute limit is per token, so sharing
  one with an MCP session makes the two compete.
- **There is no `logs -f`.** Live streaming uses a WebSocket authenticated by a browser session,
  which an API token can't open. `logs --wait-for` covers the usual reason for one: waiting for
  a line such as `listening` after a deploy.
- A dirty working tree still builds; the tag gets a `-dirty-<HHMMSS>` suffix so two different
  trees never share a tag. `tag.require_clean: true` refuses instead.
