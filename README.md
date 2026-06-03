# Krill

A minimal self-hosted PaaS written in Go: deploy containerized apps and managed databases onto a single-node Docker Swarm, routed by Traefik, managed from a dark web control plane.

> **Status:** Early, active development — a learning project. Phases 0, 1, 2, 2.5, and 3 are complete. Next up are backups (Phase 4) and domains/TLS (Phase 5). Not yet production-hardened.

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

### Managed Postgres and Redis databases (Phase 3)
- Run Postgres and Redis as Swarm services backed by named volumes.
- Generated credentials and connection strings — internal (overlay-network DNS) and external (host port).
- Full lifecycle: deploy, start, stop, delete, and version change.
- Opt-in "destroy data" on delete (the named volume is only removed when explicitly requested).

### Traefik routing (Phases 0+)
- A pinned Traefik service is bootstrapped into the Swarm and watches the Swarm API.
- Apps are discovered via service labels; only labeled services are exposed.

### Dark "Acid Industrial" UI (Phase 2.5)
- Tailwind CSS v4 design system built with the standalone CLI (no Node).
- Near-black / lime palette, Space Grotesk + JetBrains Mono, dark full-width layout.
- Reusable templ components plus an i18n foundation (`i18n.T(ctx, "key")` with an English catalog).

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

State lives in PostgreSQL; there is no clustering. Swarm is always single-node, and all control-plane operations are serialized.

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
| `KRILL_LOG_LEVEL` | `info` | No | Log level: `debug` / `info` / `warn` / `error`. |
| `KRILL_LOG_FORMAT` | `text` | No | Log handler format: `text` or `json`. |
| `KRILL_ACME_EMAIL` | `` | No | Let's Encrypt contact email (falls back to `KRILL_ADMIN_EMAIL`). |
| `KRILL_ACME_STAGING` | `false` | No | Use the Let's Encrypt staging CA (for testing without rate limits). |

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

## Data model

State is stored in PostgreSQL across ten tables, created by embedded migrations (`internal/database/migrations/`) and queried via sqlc-generated code (`internal/database/gen/`).

| Table | Notes |
|-------|-------|
| `users` | `email` unique, bcrypt `password_hash`. |
| `sessions` | Session token → user, with `expires_at` (TTL 7 days). |
| `organizations` | `slug` unique, `owner_id`. |
| `members` | `role` ∈ {owner, admin, member}; unique `(organization_id, user_id)`. |
| `projects` | Unique `(organization_id, slug)`. |
| `environments` | Unique `(project_id, slug)`. |
| `applications` | Image/tag/domain/port/status, `source_type` ∈ {image, dockerfile}, env as JSONB. |
| `deployments` | Immutable history; `status` ∈ {running, done, error}, `trigger` ∈ {manual, webhook, schedule}, with logs. |
| `postgres_dbs` | Managed Postgres service: `app_name` unique, credentials, optional `external_port`, default image `postgres:17`. |
| `redis_dbs` | Managed Redis service: `app_name` unique, password, optional `external_port`, default image `redis:7`. |

**Tenancy chain:** `organizations → projects → environments → applications / postgres_dbs / redis_dbs`. Foreign keys cascade on delete, and every resource handler chain-checks ownership (org → project → environment → resource), returning **404** on any cross-tenant mismatch.

## Roadmap & status

Phases 0–3 (plus the 2.5 UI foundation) are complete and E2E-verified. See [ROADMAP.md](ROADMAP.md) for the full plan.

What's next:

- **Phase 4 — DB backups to S3:** `destinations` (S3/MinIO) and `backups` (cron + retention); scheduled `pg_dump` / redis-dump uploaded via AWS SDK v2; manual trigger + restore.
- **Phase 5 — Domains, TLS, routing:** multiple domains per app, path routing, HTTP→HTTPS, Let's Encrypt ACME via Traefik (plus custom certs). This is the critical path for the bot, which needs **HTTPS for its webhook URL**.

Later phases cover GitHub auto-deploy (Phase 6), container logs / web terminal / metrics, and flexible state placement. A second i18n locale is post-MVP (the infrastructure already exists from Phase 2.5).

## Documentation

- [PLAN.md](PLAN.md) — current working plan.
- [ROADMAP.md](ROADMAP.md) — phase roadmap and status.
- [`docs/superpowers/specs/`](docs/superpowers/specs/) — per-phase design docs (e.g. [`2026-06-02-krill-phase3-managed-databases-design.md`](docs/superpowers/specs/2026-06-02-krill-phase3-managed-databases-design.md)).
- [`docs/superpowers/plans/`](docs/superpowers/plans/) — per-phase implementation plans.
- [CLAUDE.md](CLAUDE.md) — conventions and guidance for contributors and AI agents.

## Conventions

- **Language:** all code — comments, log messages, error strings — is written in English. Only the planning docs (`PLAN.md`, `ROADMAP.md`, `docs/superpowers/**`) are kept in Russian.
- **Commits:** Conventional Commits (`feat(scope): …`, `fix(scope): …`). No `Co-Authored-By` / attribution trailers.
- **i18n:** all user-facing strings go through `i18n.T(ctx, "key")`; add a locale by adding a translation map, not by editing templates.
- **Errors:** never swallow errors in handlers — log and/or return 500.

See [CLAUDE.md](CLAUDE.md) for the full conventions and the spec → plan → execute workflow.
