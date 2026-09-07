# Krill

A minimal self-hosted PaaS written in Go: deploy containerized apps and managed databases onto a single-node Docker Swarm, routed by Traefik, managed from a dark web control plane.

> **Status:** Active development — a learning project, running in production for the author's bot. Phases 0–7 are complete (image apps, Dockerfile builds, projects/RBAC, managed Postgres/Redis, S3 backups, domains/TLS, and realtime ops: Telegram notifications, web terminal, monitoring, structured log viewer) plus **Phase 6 — GitHub auto-deploy**. The **deploy-parity** track (gaps vs Dokploy for deploying a real private app) is fully closed, and **multi-server** (Swarm worker nodes joined over SSH, with placement) plus **cluster-wide monitoring** work too. Managed databases were rebuilt around org-level **DB instances** holding env-scoped **logical databases** (replacing one container per database); a new admin **Topology** page draws the live cluster map with app↔DB threads and an ingress lane; and **backups** now use schedule presets (including on-demand) with inline storage setup. Still open: Phase 8 (flexible state placement) and email/Slack channels. Not yet broadly production-hardened.

## Why Krill

Krill exists to deploy and manage a webhook Telegram bot (and similar small apps) on a modest VPS without the overhead of a heavyweight platform.

On a 2 vCPU / 1.9 GB RAM box, a Dokploy control plane (Node.js SSR plus native modules) consumes roughly **760 MB** of RAM — the single largest process on the machine. Krill aims for a control plane in the **~30–50 MB** range by shipping a single compiled Go binary with no Node, no npm, and no SSR runtime.

The secondary goal is educational: building a lightweight PaaS from first principles — Swarm orchestration, reverse-proxy routing, multi-tenancy, and live deploy logs — end to end.

## What works today

Features below are grouped by capability and tied to the phase that delivered them. All phases listed are E2E-verified.

### Deploy from a prebuilt image (Phase 0)
- Create an app from a ready image (e.g. `nginx:alpine`), expose a port, and get a service running on Swarm.
- Auto-generated domain via [sslip.io](https://sslip.io) (`<app>.127-0-0-1.sslip.io` by default), routed through Traefik.
- Live deploy logs streamed to the UI over WebSocket; live status badges.
- Change the image tag and trigger a zero-downtime rolling update.

### Build from a Dockerfile, with history (Phase 2)
- Point an app at a Git repo; Krill does a shallow `git clone` and a local `docker build` (single node, no registry required) before deploying.
- Single-worker build queue (one deployment at a time) to avoid Docker API contention.
- Live build logs over WebSocket; a bounded log buffer (head + tail) with a truncation marker for very long output.
- A `deployments` history (image- and Dockerfile-based), with logs auto-cleaned after one hour (metadata retained).

### Projects, organizations, environments, and RBAC (Phase 1)
- Tenancy hierarchy: **Organization → Project → Environment → App / Database**.
- Three-tier RBAC: `member` < `admin` < `owner`, enforced in middleware.
- Path-scoped routes with strict cross-tenant isolation (ownership chain-checks return 404 on mismatch).
- Per-app environment variables with in-UI editing.
- A default organization is bootstrapped on first run (the seed admin becomes its owner).

### Managed databases: DB instances + logical databases (Phase 3)
- Org-level **DB instances** — one Swarm service per Postgres or Redis server, backed by a named volume, optionally pinned to a cluster node.
- A Postgres instance holds env-scoped **logical databases** — a database plus an owner user, provisioned synchronously inside the instance's container (`CREATE DATABASE`/`CREATE USER`, public connect revoked).
- Redis instances serve apps directly — no logical sub-resource, one instance is one connection target.
- Generated credentials and connection strings — internal (overlay-network DNS) and external (host port).
- Full lifecycle on the instance: deploy, start, stop, delete, and version change; logical databases are created/deleted independently within their instance.
- Opt-in "destroy data" on delete (the named volume is only removed when explicitly requested).

### Deploy-parity: container settings, lifecycle, route exposure
- **Advanced container settings** — per-app memory/CPU limits (`256m` / `0.5`), replica count, restart policy, and a healthcheck (command/interval/timeout/retries/start period), edited on the app's **Advanced** tab and applied to the Swarm service spec.
- **App lifecycle controls** — Deploy / Reload / Rebuild / Stop buttons on the General tab. Reload restarts the service without rebuilding, Rebuild does a no-cache `docker build`, Stop scales to zero.
- **Per-domain route exposure (internal by default)** — new apps are not publicly routed; each domain has an **Exposed** toggle and an optional list of public path prefixes. Unexposed services stay on the overlay network only (no Traefik route); exposed domains can be narrowed to specific paths.
- **Private registries** — org-scoped registry credentials, selectable per app, for pulling private images (see [Private images](#private-images-registries)).
- **Environment editor** — per-app env vars edited in either a Key-Value grid or a Raw `KEY=value` text mode.
- **App volumes + backup/restore** — named-volume mounts per app (Volumes tab); volumes back up to S3 (a pinned busybox sidecar tars the volume → gzip → S3, with count retention) and restore (quiescing the app first).
- **DB→app linking** — link an app to a managed DB in the same environment; Krill injects the live internal connection string into a chosen env var on every deploy (the password is pulled live, never stored in plain env text).
- **Raw TCP/UDP published ports** — publish host ports straight into a container (host publish mode) for non-HTTP services (Gitea SSH, mail, game/DNS/VPN), independent of Traefik and the overlay HTTP port.
- **Build secrets/args + private Git** — org-scoped Git credentials (PAT) for private-repo clones, plus non-secret `--build-arg`s and BuildKit `--secret`s for Dockerfile builds (secrets encrypted at rest, never in the build context).
- **Per-domain route protection** — basic-auth (htpasswd) and/or IP-allowlist (CIDR) middleware on exposed domains — the "who" axis, orthogonal to the path-exposure "what" axis.
- **Container command override** — an optional CMD args override on the Advanced tab (e.g. `start-dev` for Keycloak), keeping the image ENTRYPOINT.

### GitHub auto-deploy (Phase 6)
- Per-app, opt-in auto-deploy via **webhook + PAT** (no OAuth / GitHub App needed).
- **Dockerfile/git apps:** a GitHub push webhook (HMAC-verified `X-Hub-Signature-256`, branch-matched) — push to the configured branch and Krill builds + deploys on the host.
- **Image apps:** a generic deploy-hook your CI calls *after* `docker push` (Bearer token, optional `?tag=`) — Krill resolves the image digest, re-pulls, and redeploys (so a same-tag push is actually picked up).
- Enable/disable, masked secret reveal/copy, and regenerate on the app's General tab; webhook-triggered deploys show `trigger=webhook` in the history.

### Realtime & operations (Phase 7)
- **Telegram notifications** — org-scoped alerts on deploy/backup failures and app down/recovered health transitions (no success spam); the bot token is encrypted and masked.
- **Web terminal** — admin-only interactive `docker exec` into a running app container (xterm + WebSocket), with a configurable idle timeout.
- **Structured log viewer** — app/DB runtime logs as a time | level | message table with text search + a level filter; deploy/build logs and the interactive terminal use xterm.
- **Monitoring** — CPU/memory over time: uPlot charts + a per-component table grouped by control/infra/app/db, sampled into Postgres on an interval with retention.

### Multi-server cluster + cluster-wide monitoring
- Join Swarm **worker nodes over SSH** from the admin **Nodes** page — one control plane + N workers (not N installs); drain/remove nodes (the control plane is guarded); per-app placement (any / pinned / global).
- Managed DBs can be pinned to a chosen node (node-local volume; apps reach them over the overlay DNS).
- **Cluster-wide monitoring** — the metrics sampler tunnels each worker's Docker socket over the stored SSH access and collects per-node stats (no agent, no exposed port); the Monitoring page shows every node, with a node selector that filters both the charts and the table.

### Cluster topology
- An admin-gated **Topology** page (`/orgs/{id}/topology`) draws the cluster as node lanes holding each org's apps and DB instances, placed where they actually run.
- Logical databases render as chips inside their Postgres instance; colored threads connect an app to the databases it uses (color by engine — Postgres/Redis — and thicker/dashed for a cross-node link).
- An **Ingress lane** shows Internet → Traefik → exposed app, with the domain labelled on the edge and arrows showing request direction.
- Connections are detected both from modeled DB links and from raw `env_text` connection strings, so an app wired by a plain DSN still shows a thread; multiple per-field links to the same database collapse into one.

### Agent & CI API (MCP + REST)
- Twelve operations for agents and pipelines — status, logs, deployment history, deploy/rebuild/reload/stop, single-variable env edits — over an **MCP server** (`/mcp`) and a **REST API** (`/api/v1`), both behind org-scoped read/write bearer tokens issued from Settings.
- No destructive operations at all (no create/delete of anything, no container shell), env values are never returned, and a token's rights are re-resolved from the owner's current role on every request. See [Agents & CI](#agents--ci-rest-api--mcp).

### Secrets at rest
- Set `KRILL_SECRET_KEY` to encrypt stored secrets at rest (AES-256-GCM): DB passwords, registry/Git/destination credentials, webhook + notification tokens. Empty key = legacy plaintext (with a startup warning); previously-plaintext values stay readable after a key is added.

### Traefik routing (Phases 0+)
- A pinned Traefik service is bootstrapped into the Swarm and watches the Swarm API.
- Apps are discovered via service labels; only labeled services are exposed.

### Dark "Acid Industrial" UI (Phase 2.5)
- Tailwind CSS v4 design system built with the standalone CLI (no Node).
- Near-black / lime palette, Space Grotesk + JetBrains Mono, dark full-width layout.
- Reusable templ components plus a bilingual i18n layer (`i18n.T(ctx, "key")` with English + Russian catalogs; language switcher in Settings).
- Post-redirect-get flash toasts (success/error) on every form action, copy buttons, named destructive confirmations, button loading states, `hx-boost` navigation, and a blurred-backdrop org-switcher modal. Destinations and Registries live under a single sidebar **Settings** group.

## Install on a server (VPS)

On a fresh Linux VPS (amd64 or arm64), one line installs Krill as a host binary
managed by systemd, alongside its own Postgres and an initialized Docker Swarm:

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
```

The installer is idempotent — **re-run it to upgrade** (it pulls the latest
release binary and restarts the service; your Postgres and secrets are kept). It:

1. installs Docker (via `get.docker.com`) if missing and initializes a single-node Swarm;
2. runs a loopback-only `postgres:17-alpine` container for Krill's own state;
3. downloads the `krill` binary from the latest GitHub Release (SHA-256 verified);
4. writes `/etc/krill/krill.env` (generated admin password + encryption key, created once);
5. installs and starts a `krill` systemd service;
6. prints the admin URL and one-time login.

Useful overrides (prefix the command): `KRILL_VERSION=v0.1.0` pins a release,
`KRILL_DOMAIN=apps.example.com` sets the base domain, `KRILL_ACME_EMAIL=...` sets
the Let's Encrypt contact, `KRILL_ADVERTISE_ADDR=...` overrides the Swarm address,
`KRILL_BINARY=/path/to/krill` installs a binary already on the host (skips the
download — handy when the repo/release is private; `scp` the binary up first),
`KRILL_SKIP_VERIFY=1` allows installing a downloaded binary when the release has
no `checksums.txt` (by default the installer refuses — fail closed).

### Use a managed / external Postgres (optional)

By default the installer runs a small loopback-only Postgres container on the
host for Krill's own state. To keep state off the box (managed durability and
backups), point Krill at an external Postgres by passing `KRILL_DATABASE_URL` to
the installer — the local container is then skipped:

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh \
  | KRILL_DATABASE_URL='postgres://user:pass@db.example.com:5432/krill?sslmode=require' sudo -E sh
```

- `sudo -E` passes the variable through to the script under `sudo`.
- Managed providers (Neon, Supabase, RDS, …) require TLS — include
  `sslmode=require` (or `verify-full`) in the DSN.
- Point at an **empty** database you created at the provider; Krill applies its
  schema automatically on first start.
- The installer verifies connectivity before continuing and aborts on failure.
- State-DB backups are then the provider's responsibility. The default local
  mode is unchanged, and re-running to upgrade keeps using the external DSN.
- This choice is made at **first install**. Re-running the installer on an
  existing local install with `KRILL_DATABASE_URL` set does **not** move state
  to the external DB (upgrades preserve the existing store) — switching backends
  needs a manual dump/restore.

After it finishes: point an A record at the server, then set the base domain and
put the admin UI behind HTTPS (set `KRILL_COOKIE_SECURE=true` in `/etc/krill/krill.env`
and `systemctl restart krill`). Logs: `journalctl -u krill -f`.

**Uninstall:**

```sh
systemctl disable --now krill
rm -f /etc/systemd/system/krill.service /usr/local/bin/krill
rm -rf /etc/krill
docker rm -f krill-postgres && docker volume rm krill-pg-data   # destroys Krill's state
```

Prefer a container instead of the host binary? The multi-arch image is published
at `ghcr.io/proshik/krill` (see [Docker & CI](#docker--ci)).

See the [installer design spec](./docs/superpowers/specs/2026-06-09-krill-installer-design.md)
for the full rationale.

## Architecture

Krill is a single Go binary (the control plane) that drives a single-node Docker Swarm over the Docker API. It stores all of its own state in PostgreSQL and lets Traefik handle HTTP routing to deployed services.

The control plane creates and updates Swarm services with label metadata. Traefik watches the Swarm API and auto-configures routes from those labels. A single deployment worker goroutine serializes builds and deploys; live logs are streamed through an in-memory hub (head + tail buffer) and persisted when a job finishes.

```
                ┌──────────────────────────────────────────────┐
   browser ───▶ │  Krill control plane (Go binary, HTTP :8080)  │
                │   - chi router + templ/HTMX UI                 │
                │   - PostgreSQL state store (pgxpool)           │
                │   - single deployment worker + log hub         │
                └───────────────────┬──────────────────────────┘
                                    │ Docker API (unix socket / DOCKER_HOST)
                                    ▼
                ┌──────────────────────────────────────────────┐
                │  Docker Swarm (single node)                    │
                │   - overlay network "krill-net"                │
                │   - Traefik (v3.6.1) reverse proxy, host :80   │
                │   - app services   krill-<appID>      (VIP)    │
                │   - db services    <app_name>         (DNSRR)  │
                │     + named volumes <app_name>-data            │
                └───────────────────┬──────────────────────────┘
                                    │ Host header match
                                    ▼
                          host port 80 / domain routing
```

State lives in PostgreSQL, and all control-plane operations are serialized through a single deploy worker. The control plane itself is a single node (no HA); the Swarm starts single-node but can be **scaled out** — additional worker nodes are joined over SSH from the **Nodes** page, and apps spread across them via placement (any / pinned / global). Stateful services (managed DBs, app volumes) stay node-local.

## Tech stack

- **Language:** Go 1.26.1
- **HTTP router:** chi/v5 (v5.3.0)
- **Templates / frontend:** templ (v0.3.1020), HTMX, Tailwind CSS v4 (v4.3.0, standalone CLI — no Node)
- **Database driver:** pgx/v5 (v5.9.2) against PostgreSQL 16
- **Query codegen:** sqlc (v1.30.0)
- **Migrations:** golang-migrate/migrate (v4.19.1), embedded and auto-run on startup
- **Container runtime:** Docker Swarm (single node) via docker/docker (v28.5.2)
- **Reverse proxy:** Traefik v3.6.1 (pinned)
- **Auth:** bcrypt password hashing (`golang.org/x/crypto` v0.52.0) + cookie sessions
- **Config:** caarlos0/env (v11.4.1)
- **WebSockets:** coder/websocket (v1.8.14)
- **Testing:** testcontainers-go (v0.42.0) with the Postgres module

## Project layout

```
cmd/krill/main.go            Startup wiring: config, migrations, services, Traefik bootstrap, HTTP server
```

| Package | Responsibility |
|---------|----------------|
| `internal/config` | Parse runtime configuration from `KRILL_*` environment variables (caarlos0/env). |
| `internal/server` | chi router, middleware chain, and request handlers for the multi-tenant URL hierarchy. |
| `internal/auth` | Authentication, cookie sessions, bcrypt hashing, RBAC role hierarchy/middleware, and admin/default-org seeding. |
| `internal/org` | Organization → project → environment hierarchy, membership, and slug uniqueness. |
| `internal/docker` | Abstraction over the Docker Swarm API: service lifecycle, image pulls, state polling, log streaming. |
| `internal/deploy` | Deployment queue + single worker, build/deploy/converge pipeline, live-log hub, status derivation, log cleanup. |
| `internal/builder` | Git clone, input validation (RCE / path-traversal guards), and local `docker build`. |
| `internal/dbservice` | Managed database lifecycle (Postgres, Redis), connection-string generation, service specs. |
| `internal/traefik` | Traefik bootstrap (pinned `TraefikVersion = "v3.6.1"`) and dynamic routing-label generation. |
| `internal/web` | templ templates, HTMX/Tailwind assets, embedded static files, and the i18n catalog. |
| `internal/database` | Embedded SQL migrations, sqlc-generated query wrappers (`gen/`), and pgx pooling. |
| `internal/secret` | AES-256-GCM encryption-at-rest for stored secrets (opt-in via `KRILL_SECRET_KEY`). |
| `internal/backup` | S3/MinIO storage + scheduled `pg_dump`/restore (cron, retention, schedule presets). |
| `internal/volume` | App-volume archive/restore to S3 (pinned busybox sidecar). |
| `internal/metrics` | Monitoring sampler (local + SSH-tunnelled per-node stats), `metric_samples`/`node_capacity` store, chart bucketing. |
| `internal/cluster` | SSH join of Swarm worker nodes (`DialVerified`, host-key handling). |
| `internal/notify` | Telegram notification channels + background health watcher. |
| `internal/webhook` | Pure HMAC verify / push-payload parse / secret helpers for GitHub auto-deploy. |
| `internal/topology` | Pure cluster-topology graph builder (nodes/services/DB links, `env_text`-DSN detection) for the Topology page. |
| `internal/testutil` | Ephemeral Postgres test databases via testcontainers. |

## Getting started

### Prerequisites

- **Go 1.26.1**
- **Docker** — on macOS, [Colima](https://github.com/abiosoft/colima) works as a Docker Desktop alternative.
- **Docker Swarm**, initialized as a single node:
  ```bash
  docker swarm init
  ```
- **Tailwind CSS v4 standalone binary** (no npm/Node), downloaded into `./tools/tailwindcss`:
  ```bash
  ./tools/get-tailwind.sh
  ```
  The script pins Tailwind `v4.3.0` and supports macOS and Linux (arm64 / x86_64). The binary is gitignored, and the build/run targets invoke it via `make generate`, so download it before your first `make run`.

### Environment

Configuration is read from `KRILL_*` environment variables (see `.env.example`).

| Variable | Default | Required | Purpose |
|----------|---------|----------|---------|
| `KRILL_LISTEN_ADDR` | `:8080` | No | HTTP server bind address. |
| `KRILL_DATABASE_URL` | — | **Yes** | PostgreSQL connection string. |
| `KRILL_ADMIN_EMAIL` | — | **Yes** | Seed admin email (created on startup if missing). |
| `KRILL_ADMIN_PASSWORD` | — | **Yes** | Seed admin password. |
| `KRILL_DOCKER_HOST` | — | No | Docker daemon socket/host (e.g. a Colima socket path). |
| `KRILL_BASE_DOMAIN` | `127-0-0-1.sslip.io` | No | Domain suffix for deployed apps (via sslip.io). |
| `KRILL_NETWORK` | `krill-net` | No | Swarm overlay network name. |
| `KRILL_HOST` | `localhost` | No | Public hostname used in generated external DB connection strings. |
| `KRILL_COOKIE_SECURE` | `false` | No | Set the `Secure` flag on session cookies. |
| `KRILL_TRUST_PROXY` | `false` | No | Trust `X-Forwarded-For` when identifying the client IP (login rate limiting). Enable **only** behind a reverse proxy that sets the header — otherwise a client can forge it and get its own rate-limit bucket. Without it, every request behind a proxy shares one bucket. |
| `KRILL_LOG_LEVEL` | `info` | No | Log level: `debug` / `info` / `warn` / `error`. |
| `KRILL_LOG_FORMAT` | `text` | No | Log handler format: `text` or `json`. |
| `KRILL_ACME_EMAIL` | `` | No | Let's Encrypt contact email (falls back to `KRILL_ADMIN_EMAIL`). |
| `KRILL_ACME_STAGING` | `false` | No | Use the Let's Encrypt staging CA (for testing without rate limits). |
| `KRILL_SECRET_KEY` | `` | No | Enables AES-256-GCM encryption-at-rest of stored secrets (empty = plaintext + a startup warning). |
| `KRILL_PUBLIC_URL` | derived | No | Externally reachable base URL shown for webhook URLs (defaults to `scheme://KRILL_HOST`, scheme from `KRILL_COOKIE_SECURE`). |
| `KRILL_ADVERTISE_ADDR` | — | No | Swarm advertise address used when joining worker nodes (multi-server). |
| `KRILL_CONVERGE_TIMEOUT` | `180s` | No | Deploy convergence cap (auto-extended by a healthcheck's start period). |
| `KRILL_MIGRATE_TIMEOUT` | `30m` | No | Caps a DB-instance volume migration (stop → copy → redeploy). |
| `KRILL_HEALTH_POLL_INTERVAL` | `30s` | No | How often the notification watcher polls service health. |
| `KRILL_TERMINAL_IDLE_TIMEOUT` | `15m` | No | Closes an idle web-terminal session (`0` disables). |
| `KRILL_METRICS_INTERVAL` | `30s` | No | How often the monitoring sampler records container stats. |
| `KRILL_METRICS_RETENTION` | `48h` | No | How long metric history is kept before pruning. |
| `KRILL_METRICS_NODE_TIMEOUT` | `10s` | No | Per-worker timeout when SSH-tunnelling to a worker's Docker socket for cluster-wide stats. |
| `KRILL_AGENT_API_ENABLED` | `true` | No | Enables the agent-facing API — **both** the REST surface (`/api/v1`) and the MCP server (`/mcp`). `false` unmounts both. (Called `KRILL_MCP_ENABLED` before 2026-08-13.) |
| `KRILL_MCP_SESSION_TIMEOUT` | `30m` | No | Closes an idle MCP session (a client that never sent `DELETE /mcp` — a crashed agent, a finished CI job) and frees its goroutine. `0` disables the idle timeout. |

A sample `.env` for standard ports (mirrors `.env.example`):

```
KRILL_LISTEN_ADDR=:8080
KRILL_DATABASE_URL=postgres://krill:krill@localhost:5432/krill?sslmode=disable
KRILL_ADMIN_EMAIL=admin@krill.local
KRILL_ADMIN_PASSWORD=changeme
KRILL_DOCKER_HOST=unix:///Users/you/.colima/default/docker.sock
KRILL_BASE_DOMAIN=127-0-0-1.sslip.io
KRILL_NETWORK=krill-net
KRILL_COOKIE_SECURE=false
```

If ports `8080`/`5432` are busy locally, use alternates such as `:18080` and `55432` (the project's working `.env` does exactly this).

### Make targets

| Target | What it does |
|--------|--------------|
| `make generate` | Run templ generate, sqlc generate, and build minified CSS. Run after editing `.templ` or `.sql` files — generated code is committed. |
| `make css` | Rebuild minified Tailwind CSS once. |
| `make css-watch` | Rebuild CSS on change (run in a separate terminal). |
| `make build` | `make generate`, then build the binary to `bin/krill`. |
| `make run` | `make generate`, then `go run ./cmd/krill`. |
| `make test` | Run all tests with the Colima/testcontainers socket override (see Testing). |
| `make test-integration` | Run tests tagged `integration`. |
| `make tidy` | `go mod tidy`. |
| `make db-up` | Start the dev Postgres via `docker compose -f docker-compose.dev.yml up -d`. |
| `make db-down` | Stop the dev Postgres. |

### Run locally

```bash
# 1. Make sure Docker/Colima is running and Swarm is initialized
docker ps
docker swarm init      # if not already initialized

# 2. Start the dev Postgres (postgres:16-alpine, user/pass/db = krill)
make db-up

# 3. First time only: download the Tailwind binary
./tools/get-tailwind.sh

# 4. Run the server (regenerates code, then starts)
make run
```

On startup the binary loads config, runs embedded migrations, seeds the admin user (and a `Default` org owned by that admin on first run), bootstraps the Traefik service, and listens on `KRILL_LISTEN_ADDR` (logs `krill listening addr=:8080`).

Then open the UI:

- **URL:** `http://localhost:8080` (or `:18080` if you chose the alternate port)
- **Default admin login:** `admin@krill.local` / `changeme` — these come from `KRILL_ADMIN_EMAIL` / `KRILL_ADMIN_PASSWORD`; change them.

## Testing

Run the full suite:

```bash
make test
```

> **Colima / testcontainers requirement (read this first).**
> On Colima, the Docker socket lives on a virtiofs filesystem that Ryuk (the testcontainers cleanup service) cannot mount, which breaks test teardown and causes false failures. Tests must run with **both** of these set:
> ```bash
> DOCKER_HOST=unix://$HOME/.colima/default/docker.sock        # host socket path
> TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock   # in-VM socket path
> ```
> The `make test` target sets both automatically — prefer it over a bare `go test ./...`.

Integration-tagged tests only:

```bash
make test-integration
```

An end-to-end smoke test lives at `scripts/e2e.sh`. It spins up an isolated Postgres (`:55432`), Krill (`:18080`), and Traefik (host `:80`), then exercises login, RBAC, image- and Dockerfile-based deploys, rolling updates, Traefik routing, and a managed Postgres with an external-port `SELECT 1`. It self-cleans on exit (via an EXIT trap) and returns a nonzero exit code if any assertion fails.

## Docker & CI

### Container image

The control plane ships as a single static Go binary on a [distroless](https://github.com/GoogleContainerTools/distroless) base — about **25 MB**. SQL migrations and web assets are embedded via `//go:embed`, so the runtime image carries nothing but the binary; it needs only a reachable PostgreSQL and access to a Swarm-enabled Docker daemon.

```bash
# Build (uses the committed generated files; does NOT run `make generate`)
docker build -t krill:local .

# Run (point at your Postgres + Docker socket; serves on :8080)
docker run --rm -p 8080:8080 \
  -e KRILL_DATABASE_URL='postgres://krill:krill@db-host:5432/krill?sslmode=disable' \
  -e KRILL_ADMIN_EMAIL=admin@krill.local \
  -e KRILL_ADMIN_PASSWORD=changeme \
  -e KRILL_DOCKER_HOST=unix:///var/run/docker.sock \
  -v /var/run/docker.sock:/var/run/docker.sock \
  krill:local
```

### GitHub Actions

- **`.github/workflows/ci.yml`** — every push (feature branches **and** `master`) and PR: `go build` / `go vet` / `go test` (testcontainers runs against the runner's native Docker — no socket override needed), a no-push image build that validates the `Dockerfile`, and a "generated up to date" check (`make generate` then `git diff --exit-code`).
- **`.github/workflows/release.yml`** — on a `v*.*.*` tag push (or manual `workflow_dispatch`): builds a multi-arch image (`linux/amd64` + `linux/arm64`), pushes it to `ghcr.io/<owner>/krill`, and (for tag pushes) creates a GitHub Release.

```bash
# Cut a release
git tag v0.1.0 && git push origin v0.1.0
```

The workflows use the built-in `GITHUB_TOKEN` (no extra secrets). The GHCR package is created **private** on first push — change it to public in the package settings if you want anonymous pulls.

## Domains & HTTPS

Each app starts with one auto-generated domain (`<name>.<KRILL_BASE_DOMAIN>`). On the app's **Domains** tab you can add custom domains and toggle HTTPS per domain:

1. Add the domain (e.g. `bot.example.com`).
2. Point an `A`/`AAAA` DNS record for it at the server's public IP.
3. Enable **HTTPS** on that domain.

Traefik then obtains a Let's Encrypt certificate via the HTTP-01 challenge and serves the domain on `:443`, redirecting `http://` → `https://`. Certificates are stored in the persistent `krill-traefik-acme` volume (so restarts don't re-request them and risk rate limits). Issuance is asynchronous — if a cert doesn't appear, check the `krill-traefik` service logs and verify DNS + that ports 80/443 are reachable. Set `KRILL_ACME_STAGING=true` while testing to use the staging CA. Domains without HTTPS enabled (e.g. the local sslip.io one) keep working over plain HTTP — ACME is never attempted for them.

## Backups

Logical **Postgres** databases and app volumes can be backed up to S3-compatible storage (AWS S3 or MinIO), through the same flow for both.

1. Add S3/MinIO **storage** (name, endpoint — blank for AWS, a URL for MinIO — bucket, region, access/secret keys) from the org's **Storage** page, or inline — the backup section shows an add-storage form right there when the org has none yet, and returns you to where you started.
2. On a logical database's detail page (or an app's Volumes tab), open the **Backups** section and add a backup: pick the storage, a schedule — **On-demand / Hourly / Daily / Weekly / Monthly / Custom (cron)** — and how many backups to keep. The raw cron field only shows up under **Advanced**, alongside the optional S3 key prefix.
3. Backups run on schedule (in-process cron) or via **Backup now** at any time — including on-demand backups, which have no automatic schedule at all. Each run streams `pg_dump --clean --if-exists` from inside the instance's container → gzip → `s3://<bucket>/<prefix>/<app_name>/<db_name>/<timestamp>.sql.gz` (volumes: a pinned busybox sidecar tars the volume instead), then prunes to the newest N.
4. **Restore** any listed file (streams it back through `psql`, or untars it for a volume). Restore **overwrites** the target, so it asks for confirmation. **Download** streams the file through Krill. Pause/Resume toggles a backup's schedule.

Notes: this is a UX layer over the same engine as before (same S3/cron transport, same retention) — only the labels and flow changed. S3 keys are stored in plaintext (like DB passwords); restore requires the target to be running; the local sslip.io setup needs no backups config. The full `pg_dump`/restore roundtrip is verified on a real host (it needs a running DB container + reachable S3); the S3 paths and config are covered by tests.

## Private images (registries)

To deploy from a **private** registry image, add registry credentials and select them on the app:

1. On the org, open **Registries** (admin) and add one: a name, the registry URL (e.g. `ghcr.io`, `registry-1.docker.io` for Docker Hub, `registry.gitlab.com`), a username, and a password/token. The credentials are validated against the registry on save.
2. On the app — in the create dialog or the **General** tab — pick that registry (default is "Public (no auth)").
3. Deploy: Krill passes the encoded auth to Swarm (`--with-registry-auth`), so the private image is pulled with your credentials.

Notes: credentials are stored plaintext (like other secrets); a single registry per app; token-only registries that need dynamic credentials (AWS ECR, GCP Artifact Registry) are not yet supported — use a static username + password/token (covers GHCR PATs, Docker Hub, GitLab). The live private pull is verified on a real host.

## Agents & CI (REST API + MCP)

Krill exposes twelve operations for AI agents and CI over two surfaces backed by the same code: **REST/JSON at `/api/v1`** for scripts and pipelines, and an **MCP server (streamable HTTP) at `/mcp`** for agents. Both authenticate with the same org-scoped bearer token and run the same tenancy checks.

They **operate apps that already exist** — status, logs, deploy/rebuild/reload/stop, and single-variable env edits. There are no destructive operations at all: nothing creates or deletes an organization, project, environment, app, database, domain or volume, and there is no `docker exec`/terminal. The blast radius is bounded by the surface itself rather than by a permission matrix — a list of twelve operations can be checked by eye; a permission matrix cannot.

### 1. Issue a token

Open the org's **Settings → API tokens** page and create one: a name, a level (**read** or **write**), and an expiry (never / 30 days / 90 days). Creating a token is admin-only; any member can list and revoke their own.

- The plaintext (`krill_pat_…`) is shown **exactly once**, right after creation. It is never recoverable — the database stores only a SHA-256 hash and the 8-character lookup prefix. Lost it? Revoke and issue a new one.
- Rights are resolved **live on every request**: the effective right is the token's level intersected with the owner's *current* role in that org. Demote the owner to `member`, or remove them from the org, and their write token stops writing immediately — no separate revocation step. It keeps reading as long as membership holds.
- A token is bound to one organization. An app in another org reports as *not found*, never *forbidden*.

### 2. Connect an agent (MCP)

```bash
claude mcp add --transport http krill https://krill.example.com/mcp \
  --header "Authorization: Bearer krill_pat_..."
```

Read-level tools: `krill_whoami`, `krill_list_apps`, `krill_app_status`, `krill_app_logs`, `krill_deployments`, `krill_deployment_status`, `krill_list_env`.
Write-level tools: `krill_deploy`, `krill_rebuild`, `krill_reload`, `krill_stop`, `krill_set_env`.

Apps are addressed by `project/environment/app` path or numeric id — `krill_list_apps` returns both.

### 3. Call it from CI (REST)

The token goes in the `Authorization` header and **only** there — never `?token=`, because query strings land in reverse-proxy logs and browser history.

```bash
# Deploy an image app at a new tag, then poll until it finishes
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
| `GET /api/v1/whoami` | read | User, org, level, live role, `can_write`. |
| `GET /api/v1/apps` | read | Every app in the org: path, id, status, source type, image repository + tag, domains. |
| `GET /api/v1/apps/{app}` | read | One app: status, `running/desired` replicas, node, domains, last deployment. `image` is the repository and `tag` the configured tag (image apps only). |
| `GET /api/v1/apps/{app}/logs` | read | Runtime log tail. `?tail=` (default 200, max 1000), `?level=` (`trace`…`fatal`). |
| `GET /api/v1/apps/{app}/deployments` | read | History, newest first. `?limit=` (default 20, max 50). |
| `GET /api/v1/deployments/{id}` | read | One deployment plus the last ~8 KB of its build log. |
| `GET /api/v1/apps/{app}/env` | read | Variable **names** and source (`literal` / `db-link`) — values are never returned. |
| `POST /api/v1/apps/{app}/deploy` | write | Body `{"tag":"…"}` optional (image apps only) — a bare tag, e.g. `v1.2.3`, not a full image reference. Returns `{deployment_id, status}`. |
| `POST /api/v1/apps/{app}/rebuild` | write | `--no-cache` build; `dockerfile` apps only. Returns `{deployment_id, status}`. |
| `POST /api/v1/apps/{app}/reload` | write | Restart the tasks in place — same image, no build, no pull. |
| `POST /api/v1/apps/{app}/stop` | write | Scale to 0 replicas; deploy brings it back. |
| `POST /api/v1/apps/{app}/env` | write | Body `{"key":"K","value":"V"}` or `{"key":"K","remove":true}` — one line, rest untouched. The value must be single-line (encode a PEM key or JSON blob, e.g. base64). |

`{app}` is a `project/environment/app` path or a numeric id. Deploys are **asynchronous**: `deploy`/`rebuild` enqueue the job and return a `deployment_id` immediately, and the caller polls `GET /api/v1/deployments/{id}` for `running` → `done` / `error`. An env edit is written to the app's env file but does not restart anything — it takes effect on the next deploy.

### 4. Install the skill

`skills/krill-deploy/SKILL.md` in this repo teaches an agent the deploy-and-verify and diagnose-a-failure runbooks (poll on a widening interval, don't retry a failed deploy, never invent a variable's value, ask before touching production). It is meant to run from your *application's* repo, so install it globally:

```bash
cp -r skills/krill-deploy ~/.claude/skills/
```

### Notes

- **`KRILL_AGENT_API_ENABLED`** (default `true`) is a single switch over **both** surfaces — setting it to `false` unmounts `/api/v1` as well as `/mcp`, and every request to either 404s.
- **60 requests per minute per token**, then `429` with `Retry-After`. MCP spends that budget faster than REST: a session costs an `initialize` and a `tools/list` before the first real call.
- **`401` means the credential was rejected; `503` (`code: "unavailable"`) means it could not be checked at all** (the database behind authentication is down). Only the first is a reason to reissue a token — retry the second.
- `krill_list_env` / `GET …/env` return **names only, never values** — deliberately, since everything an agent reads reaches its model provider. Writing a value is allowed; reading one is not.
- Bearer tokens cross the network in clear text over plain HTTP. Krill warns at startup when the agent API is enabled and the public URL is not `https://` — put it behind TLS.
- Every write logs an INFO audit line with `token_id` and `user_id` (never the token, never an env value).
- An app whose name is itself one of the route verbs (`logs`, `env`, `deployments`, `deploy`, `rebuild`, `reload`, `stop`) can only be addressed over REST by numeric id — the path form splits on that trailing segment.

## krill-cli — build locally, deploy to Krill

`krill-cli` builds a container image **on your machine** and deploys it. It exists for when
building in CI is not an option — free build minutes run out, and a 2 vCPU / 1.9 GB VPS is
not where you want to run `docker build` either.

It deploys applications that already exist; projects, environments and applications are
created in the web UI.

```bash
make build-cli                  # → bin/krill-cli
krill-cli login --server https://krill.example.com   # token is read from stdin
cd ~/code/my-bot
krill-cli init                  # pick the app; writes krill.yaml
krill-cli deploy
```

```
$ krill-cli deploy

  app       acme/production/bot  (image)
  image     ghcr.io/proshik/bot
  tag       main-a1b2c3d4e5f6
  platform  linux/amd64
  via       registry

✓ build    18.4s
✓ push     ghcr.io/proshik/bot:main-a1b2c3d4e5f6   41.2s
✓ deploy   queued  #418
✓ rollout  1/1 running

https://bot.example.com
```

### How the image reaches the server

Set `delivery:` in `krill.yaml`:

| | What happens | Cost |
|---|---|---|
| `registry` (default) | `docker push`, then Krill pulls | Only the layers that changed cross the network — usually a few MB per deploy. Needs a registry account. |
| `upload` | **planned, not implemented** — setting it is refused before anything is built | It would stream the image straight to Krill with no registry at all, at the cost of shipping the whole image on **every** deploy rather than only the changed layers. The server side does not exist yet. |

For a private repository, Krill still needs its own pull credentials — that is the
[Registries](#private-images-registries) page, unrelated to your local `docker login`.

### `krill.yaml`

Committed with your application; it holds **no secrets** (the token lives in
`~/.config/krill/config.json`). An unknown key is a hard parse error, which is what keeps a
`token:` from quietly ending up in a commit.

```yaml
app: acme/production/bot
image:
  repository: ghcr.io/proshik/bot   # must match the app's Image in Krill
  platform: linux/amd64             # what the SERVER runs, not your Mac
build: { context: ., dockerfile: Dockerfile }
tag:   { strategy: git }            # git | timestamp
delivery: registry
```

**`platform` defaults to `linux/amd64`, not to your machine.** Building for an Apple-silicon
host and deploying to an amd64 VPS produces an image that loads fine and dies on start with
`exec format error`, visible only in the container log. If your server is arm64, say so here.

### Commands

`deploy` (alias `up`) · `init` · `login` · `context [use|rm]` · `apps` · `status` · `logs` ·
`deployments` · `deployment ID --watch` · `env [set|rm]` · `stop` · `reload` · `version` ·
`completion`

Exit codes: `0` ok · `1` the deployment failed · `2` configuration · `3` another deploy was
in flight · `4` timed out watching · `5` deployed but not running · `6` authentication.

### Notes

- **Checks run before the build, not after.** The token's level, the app's existence and
  source type, and whether `image.repository` matches what Krill actually pulls are all
  verified first — a repository mismatch otherwise produces a green deploy of the *old*
  image, which looks like the build did nothing.
- **`krill-cli` and the MCP server do not replace each other.** MCP has no binary channel,
  so an agent cannot carry an image; the CLI is where a build happens. An agent working in
  your application's repo can run `krill-cli deploy` over Bash and then verify with
  `krill_app_status` / `krill_app_logs`.
- **Use a separate token per machine.** The 60 requests/minute limit is *per token*, so a
  token shared with an MCP session makes the two compete.
- **There is no `logs -f`.** Live streaming is a WebSocket authenticated with a browser
  session cookie, which an API token cannot produce.
- A dirty working tree still builds; the tag is marked `-dirty-<HHMMSS>` so two different
  trees can never share one tag. `tag.require_clean: true` refuses instead.
- Distribution is currently `make build-cli` or `go install github.com/proshik/krill/cmd/krill-cli@latest`,
  plus release tarballs for linux/darwin × amd64/arm64. A Homebrew tap needs the repository
  to be public first.


## Data model

State is stored in PostgreSQL (24 tables as of migration `000030`), created by embedded migrations (`internal/database/migrations/`) and queried via sqlc-generated code (`internal/database/gen/`). The core tenancy tables are below; later features added `domains`, `registries`, `git_credentials`, `backups`, `destinations`, `notification_channels`, `metric_samples` + `node_capacity` (monitoring), `app_volumes` + `volume_backups`, `app_db_links`, `app_ports`, `cluster_nodes`, `db_instances`, and `logical_databases` — see [CLAUDE.md §6](CLAUDE.md) for the full list.

| Table | Notes |
|-------|-------|
| `users` | `email` unique, bcrypt `password_hash`. |
| `sessions` | Session token → user, with `expires_at` (TTL 7 days). |
| `organizations` | `slug` unique, `owner_id`. |
| `members` | `role` ∈ {owner, admin, member}; unique `(organization_id, user_id)`. |
| `projects` | Unique `(organization_id, slug)`. |
| `environments` | Unique `(project_id, slug)`. |
| `applications` | Image/tag/domain/port/status, `source_type` ∈ {image, dockerfile}, order-preserving `env_text`, advanced container limits, optional `auto_deploy` + `webhook_secret` (Phase 6). |
| `deployments` | Immutable history; `status` ∈ {running, done, error}, `trigger` ∈ {manual, webhook, schedule}, with logs. |
| `db_instances` | Org-level Postgres/Redis server: `app_name` unique (= Swarm service name = internal DNS host), optional `node_hostname` pin, optional `external_port`. |
| `logical_databases` | A database + owner user living inside a `db_instances` Postgres server; unique `(instance_id, db_name)` and `(environment_id, name)`. |

**Tenancy chain:** `organizations → projects → environments → applications`. `db_instances` hangs directly off `organizations` (org-level, not project/env-scoped); `logical_databases` has two FKs — `instance_id → db_instances` (RESTRICT, can't drop an instance that still holds databases) and `environment_id → environments` (CASCADE). Foreign keys cascade on delete except where noted, and every resource handler chain-checks ownership (org → project → environment → resource), returning **404** on any cross-tenant mismatch.

## Roadmap & status

Phases 0–7 are complete and E2E-verified, plus **Phase 6 (GitHub auto-deploy)**, the full **deploy-parity** track, **multi-server** clustering, **cluster-wide monitoring**, the managed-database rebuild onto **DB instances + logical databases**, the visual **Topology** page, and the **backup UX simplification**. The UI is bilingual (English + Russian). See [ROADMAP.md](ROADMAP.md) for the detailed, living tracker.

What's left:

- **Phase 8 — flexible state placement:** choose local Postgres/Redis or remote managed instances (via DSN) at install time, instead of always running state containers on the host.
- **Email / Slack notifications:** extend the notification channels (Telegram alerts on deploy/backup failures + app-health transitions already ship).
- Assorted deferred items: per-node load alerts and disk/network metrics, SSH deploy-keys, volume-backup encryption, and GitHub OAuth/App (the current auto-deploy uses webhook + PAT).

## Documentation

- [PLAN.md](PLAN.md) — current working plan.
- [ROADMAP.md](ROADMAP.md) — phase roadmap and status.
- [`docs/superpowers/specs/`](docs/superpowers/specs/) — per-phase design docs (e.g. [`2026-06-02-krill-phase3-managed-databases-design.md`](docs/superpowers/specs/2026-06-02-krill-phase3-managed-databases-design.md), [`2026-07-04-krill-topology-graph-design.md`](docs/superpowers/specs/2026-07-04-krill-topology-graph-design.md), [`2026-07-05-krill-backup-ux-simplification-design.md`](docs/superpowers/specs/2026-07-05-krill-backup-ux-simplification-design.md)).
- [`docs/superpowers/plans/`](docs/superpowers/plans/) — per-phase implementation plans.
- [CLAUDE.md](CLAUDE.md) — conventions and guidance for contributors and AI agents.

## Conventions

- **Language:** all code — comments, log messages, error strings — is written in English. Only the planning docs (`PLAN.md`, `ROADMAP.md`, `docs/superpowers/**`) are kept in Russian.
- **Commits:** Conventional Commits (`feat(scope): …`, `fix(scope): …`). No `Co-Authored-By` / attribution trailers.
- **i18n:** all user-facing strings go through `i18n.T(ctx, "key")`; add a locale by adding a translation map, not by editing templates.
- **Errors:** never swallow errors in handlers — log and/or return 500.

See [CLAUDE.md](CLAUDE.md) for the full conventions and the spec → plan → execute workflow.
