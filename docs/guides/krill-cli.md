# krill-cli — build locally, deploy to Krill

`krill-cli` builds a container image **on your machine** and deploys it to an existing Krill
app. It is for when building in CI isn't an option — free build minutes run out — and a
2 vCPU / 2 GB server is not where you want to run `docker build` either.

It deploys apps that already exist; projects, environments and apps are created in the web UI.

## Install

Download a release tarball for Linux or macOS (amd64 / arm64):

```sh
curl -sSL https://github.com/proshik/krill/releases/download/v0.1.0/krill-cli_v0.1.0_darwin_arm64.tar.gz \
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

`deploy` (alias `up`) · `init` · `login` · `context [use|rm]` · `apps` · `status` · `logs` ·
`deployments` · `deployment ID --watch` · `env [set|rm]` · `stop` · `reload` · `version` ·
`completion`

Exit codes: `0` ok · `1` the deployment failed · `2` configuration · `3` another deploy was in
flight · `4` timed out watching · `5` deployed but not running · `6` authentication.

## Notes

- **Checks run before the build.** The token's level, the app's existence and source type, and
  whether `image.repository` matches what Krill pulls are verified first — a repository
  mismatch would otherwise produce a green deploy of the *old* image.
- **`krill-cli` and MCP complement each other.** MCP can't carry an image, so the build happens
  in the CLI; an agent working in your repository can run `krill-cli deploy` and then check the
  result with `krill_app_status` / `krill_app_logs`.
- **Use a separate token per machine.** The 60 requests/minute limit is per token, so sharing
  one with an MCP session makes the two compete.
- **There is no `logs -f`.** Live streaming uses a WebSocket authenticated by a browser session,
  which an API token can't open.
- A dirty working tree still builds; the tag gets a `-dirty-<HHMMSS>` suffix so two different
  trees never share a tag. `tag.require_clean: true` refuses instead.
