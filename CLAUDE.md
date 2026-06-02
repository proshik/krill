# CLAUDE.md — Krill

Project-level guidance for the next AI coding agent (and developers). Read this before touching code. It exists to keep you from breaking things or wasting time.

## 1. What Krill is + status

Krill is a minimal self-hosted PaaS written in Go (`github.com/proshik/krill`) — a lightweight Dokploy alternative aimed at a ~20–50 MB control plane (vs Dokploy's ~760 MB) so the author can host a webhook Telegram bot on a 2 vCPU / 1.9 GB VPS. It deploys containerized apps and managed databases on a single-node Docker Swarm, routes traffic via Traefik, and exposes a web control plane (templ + HTMX + Tailwind v4) with a multi-tenant org → project → environment → app/db hierarchy and bcrypt/cookie auth.

Status: Phases 0, 1, 2, 2.5, 3 are DONE and E2E-verified (login, image apps, Swarm services, Traefik routing, live logs, rolling updates, Dockerfile build+deploy, deploy history, UI design system + i18n, managed Postgres/Redis with volumes and external ports). Tech-debt backlog for Phases 1–3 is fully closed (no open items). Next: Phase 4 (DB backups → S3), then Phase 5 (domains/TLS/Let's Encrypt) which is the critical path for the bot's HTTPS webhook. See [`ROADMAP.md`](./ROADMAP.md) / [`PLAN.md`](./PLAN.md) and [`docs/superpowers/`](./docs/superpowers/) for the full trail.

## 2. Build / run / test (exact commands)

THE most important gotcha — DB/integration tests REQUIRE the Colima testcontainers env. Always run tests via `make test`. It auto-sets both env vars that Ryuk (testcontainers cleanup) needs on Colima's virtiofs:

```
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock        # host socket path
TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock  # in-VM socket path
```

Without BOTH set, tests false-fail with a socket-mount error. NEVER report "Docker not running" without first trying this env. If you cannot use `make`, run the same command manually:

```
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock \
TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock \
go test ./...
```

After editing any `.templ` or `.sql` file you MUST run `make generate` — generated code (`*_templ.go`, `internal/database/gen/*.sql.go`, and `internal/web/static/app.css`) is committed to the repo and the build uses it.

Commands:

| Task | Command |
|------|---------|
| Generate code (templ + sqlc + css) | `make generate` |
| Build binary | `make build` (→ `bin/krill`) |
| Run server | `make run` |
| Rebuild CSS (minified) | `make css` |
| Watch CSS | `make css-watch` |
| Run all tests (with Colima fix) | `make test` |
| Integration tests only | `make test-integration` (`-tags=integration`) |
| Start dev Postgres | `make db-up` |
| Stop dev Postgres | `make db-down` |
| `go mod tidy` | `make tidy` |
| Download Tailwind binary (first time) | `./tools/get-tailwind.sh` |
| Full E2E smoke test | `./scripts/e2e.sh` |

Local ports: app on `:18080`, Postgres on `55432` (the `.env` config used when the standard `8080`/`5432` are busy; `.env.example` uses `8080`/`5432`). Login: `admin@krill.local` / `changeme`.

Prereqs: Go 1.26.1, Docker/Colima running, Swarm initialized (`docker swarm init`), Tailwind v4.3.0 standalone binary at `./tools/tailwindcss` (no Node/npm). On startup `cmd/krill/main.go` runs embedded migrations, seeds the admin user + default org, bootstraps Traefik + the overlay network, then serves on `KRILL_LISTEN_ADDR`.

Required env vars: `KRILL_DATABASE_URL`, `KRILL_ADMIN_EMAIL`, `KRILL_ADMIN_PASSWORD`. Optional (with defaults): `KRILL_LISTEN_ADDR` (`:8080`), `KRILL_DOCKER_HOST`, `KRILL_BASE_DOMAIN` (`127-0-0-1.sslip.io`), `KRILL_NETWORK` (`krill-net`), `KRILL_HOST` (`localhost`), `KRILL_COOKIE_SECURE` (`false`), `KRILL_LOG_LEVEL` (`info`: debug/info/warn/error), `KRILL_LOG_FORMAT` (`text`: text/json).

**Logging:** structured `slog`, configured in `cmd/krill/main.go` (`setupLogging`). One access-log line per request comes from the `requestLogger` middleware (`internal/server/logging.go`); 5xx logs at error level. In handlers use `logFrom(r)` — a `*slog.Logger` pre-tagged with `request_id`/`user_id`/`org_id` — for error logs (before any 5xx response) and INFO audit logs (before the success redirect). Never log secrets (passwords, temp passwords, tokens, connection strings).

## 3. Critical facts that bite

- **Traefik is pinned to `v3.6.1`** (`internal/traefik/bootstrap.go`, const `TraefikVersion`). Versions ≤3.6.0 hardcode Docker API 1.24, which Docker Engine 29.x rejects ("client version 1.24 is too old"), breaking the Swarm provider and all routing. v3.6.1 auto-negotiates the API version. Traefik is configured via CLI args only (no `traefik.yml`); the only mount is the docker socket (read-only).
- **Tailwind v4 is CSS-first.** There is NO `tailwind.config.js` and NO `@tailwind` directives. Config lives in `internal/web/styles/input.css` via `@import "tailwindcss"`, `@source "../templates"`, `@theme { ... }`, and `@layer components`. Regenerate with `make css` or `make generate`. The output `internal/web/static/app.css` is a committed artifact (generated, not hand-edited).
- **templ component-on-its-own-line gotcha.** A `@Component(...)` call inline after text renders as literal `@Component` text. Put component calls on their own node:
  ```
  <div>
    @StatusBadge(status)
  </div>
  ```
- **Native `<dialog>` modals need `margin: auto`.** Tailwind preflight resets `margin: 0` on `<dialog>`, pinning modals to the top-left. The `.k-modal` class in `input.css` restores `margin: auto` to center them.
- **DB log feeds use negative feed IDs to avoid colliding with positive deployment IDs.** The `DeployLogHub` lives in `internal/deploy/logs.go`; the database feed-ID functions live in `internal/dbservice/service.go`: `pgFeedID(id) = -(id*2 + 1)`, `redisFeedID(id) = -(id*2 + 2)` (exported as `PgFeedID`/`RedisFeedID` for the deploy-log WS handler).
- **Named volume `<appName>-data` persists** for managed databases unless the user opts in via the "Destroy data" checkbox (`destroy_data=on`) on delete; only then is `engine.VolumeRemove()` called.

## 4. Conventions (follow these)

- **Commit messages:** conventional-commits style (`feat(scope): ...`, `fix(scope): ...`, `test(...)`, `docs(...)`). HARD RULE: NO `Co-Authored-By` / Claude attribution trailers of any kind.
- **All UI strings go through `i18n.T(ctx, "key")`** with keys in the English catalog (`internal/web/i18n/en.go`). No raw user-facing literals in templates or handlers. The locale middleware (`WithLocale`) is currently a no-op (`en`); a second locale = a second map, no template changes.
- **Write everything in code in ENGLISH** — comments, `slog`/log messages, `fmt.Errorf`/`errors.New` strings, and `http.Error` messages. The codebase is English-only. The ONLY Russian that stays is the planning docs: `PLAN.md`, `ROADMAP.md`, and `docs/superpowers/**`. Do not introduce Russian in `.go`, `.templ`, `.sql`, `.sh`, `.css`, or config files.
- **Every app/db route must chain-check tenant ownership and 404 on mismatch.** Load org → project → environment (and db) and verify each FK matches its parent (`loadOrg` → `loadProject` → `loadEnvironment` → `loadApp`/`loadDBChain`). Mismatch returns `http.NotFound`. There are IDOR regression tests; keep them green.
- **Handlers must NOT swallow errors** — log via `slog.Error("context", "err", err)` or return 500. No silent ignores in delete/deploy/version handlers.
- **Only an owner may grant the owner role** (`updateMemberRole` in `internal/server/org_handlers.go`) — prevents admin self-escalation.
- **Deploy waits for convergence before marking done.** After `ServiceDeploy`, poll `engine.ServiceState()` until `Running >= Desired` (90s timeout, 1s interval) before `finish()`.
- **Build logs are bounded** — head (128 KB) + tail (128 KB) buffer with a `...[truncated N bytes]...` marker only when the middle is actually dropped (`internal/deploy/logs.go`).
- **RBAC is owner > admin > member** (`Role` int with `RoleMember`/`RoleAdmin`/`RoleOwner` and `AtLeast`), enforced by `RequireRole(min)` middleware (403 if role < min). Admin+ can manage projects/apps/dbs/members; members are read-only.
- **`external_port` is validated and conflict-checked** — `strconv.Atoi` + range `1..65535` → 400 on bad input; pre-check existing ports → 400 on duplicate.
- **Temp passwords flow via HttpOnly flash cookie** (`krill_flash_pw`), never in the URL; the template reads it once then clears it.
- **Unique-violation detection uses `errors.As(*pgconn.PgError)` + code `23505`** (`mapUniqueErr` in `internal/org/service.go`), not string matching.

## 5. Architecture map

Runtime topology: a single Go binary (HTTP on `:8080`/`:18080`) holds all state in PostgreSQL (pgxpool) and drives a single-node Docker Swarm via the Docker API. Traefik (a Swarm service on host `:80`) watches the Swarm API and routes by `Host` header to app/db services on the `krill-net` overlay. One deployment worker goroutine serializes all build/deploy jobs.

Packages:

- **`cmd/krill`** — entry point: load config → migrate → pgxpool + sqlc → seed admin/default org → wire services → bootstrap Traefik+network → start chi server → graceful shutdown.
- **`internal/config`** — env parsing via caarlos0/env (`Config`, `Load()`).
- **`internal/server`** — chi router, middleware chain, all handlers; path-scoped multi-tenant routes. Tenancy chain-checks `loadOrg`/`loadProject`/`loadEnvironment`/`loadApp` live in `context.go`; `loadDBChain` lives in `db_handlers.go`.
- **`internal/auth`** — sessions, bcrypt, RBAC (`Role` member<admin<owner, `AtLeast`), cookie `krill_session` (`CookieName`, `SessionTTL` = 7 days); middleware `RequireAuth`, `RequireOrgMember`, `RequireRole`; ctx helpers `UserID`/`OrgID`/`RoleOf`.
- **`internal/org`** — org/project/env/app/member CRUD, `Slugify`, `ErrNotFound`/`ErrSlugTaken`, `mapUniqueErr`.
- **`internal/docker`** — Swarm API wrapper. **`Engine` interface methods:** `NetworkEnsure(ctx,name)`, `ServiceDeploy(ctx,ServiceSpec)`, `ServiceRemove(ctx,name)`, `ServiceState(ctx,name)→ServiceState`, `ServiceLogs(ctx,name,follow)→io.ReadCloser`, `ServiceScale(ctx,name,replicas uint64)`, `ImagePull(ctx,ref,out)`, `VolumeRemove(ctx,name)`. Helpers: `ServiceName(appID)→"krill-<id>"`, `BuildImageTag(appID,deployID)→"krill-<id>:<deployID>"`. Rolling update is parallelism=1, StartFirst (zero-downtime), rollback-on-failure; DNSRR for DBs, VIP for apps.
- **`internal/deploy`** — `Deployer` (queue + 1 worker, 10-min job timeout, 90s convergence poll), `DeployLogHub` (in-memory head+tail buffer + WS subscribers), `Store` interface, status values (`idle`/`deploying`/`running`/`error`), `DeriveStatus`, `StartLogCleanup` (truncates logs >1h old).
- **`internal/builder`** — `Builder` interface; `gitBuilder` does shallow `git clone --depth=1` + `docker build`; `ValidateBuildRequest` guards against flag injection and path traversal.
- **`internal/dbservice`** — managed Postgres/Redis lifecycle over Engine+Store; service specs (DNSRR, named volume `<appName>-data`, manager constraint) in `spec.go`; connection-string builders (internal overlay DNS / external host port).
- **`internal/traefik`** — `TraefikVersion`, `TraefikSpec(network)`, `Bootstrap(ctx,engine,network)` (idempotent), `AppLabels(...)` generating `traefik.http.routers.*`/`services.*` labels.
- **`internal/web`** — embedded static (`Static()` in `embed.go`), i18n (`WithLocale`, `T`, `en` catalog), templ templates (`layout`, `login`, `orgs`, `org_dashboard`, `project`, `app_detail`, `database_detail`, `deployments`, `members`, `status_badge`, plus shared `ui/components`).
- **`internal/database`** — `RunMigrations(dsn)` (embedded, idempotent, ignores `ErrNoChange`), sqlc-generated `gen/` (read-only).
- **`internal/testutil`** — `NewTestDB(t)` spins an ephemeral `postgres:16-alpine` testcontainer, migrates, returns a pool with cleanup.

## 6. Data model

Tables (BIGSERIAL PKs unless noted): `users`, `sessions` (PK token), `organizations`, `members`, `projects`, `environments`, `applications`, `deployments`, `postgres_dbs`, `redis_dbs`. Migrations: `000001_init`, `000002_org_hierarchy`, `000003_dockerfile_deployments`, `000004_managed_databases`.

Tenancy chain (FK-enforced, ON DELETE CASCADE): `organizations` → `projects` → `environments` → `applications` / `postgres_dbs` / `redis_dbs`. `members` joins users↔orgs with `UNIQUE(org_id,user_id)` and role CHECK (`owner`/`admin`/`member`).

Key facts:
- For managed DBs, `app_name` is UNIQUE and is simultaneously the Swarm **service name = internal DNS hostname = volume name prefix** (`<appName>-data`).
- `external_port` is a nullable `*int32` (sqlc `emit_pointers_for_null_types: true`); conflicts checked via `CountPostgresByExternalPort` / `CountRedisByExternalPort`.
- DB **credentials/passwords are stored plaintext** (no encryption at rest).
- `applications.env` is JSONB, overridden in sqlc to `map[string]string`; `domain` has a UNIQUE index; `source_type` CHECK (`image`/`dockerfile`).
- `deployments` is append-only history: `status` CHECK (`running`/`done`/`error`), `trigger` CHECK (`manual`/`webhook`/`schedule`), index `(application_id, started_at DESC)`; `ClearOldDeploymentLogs` empties the `log` field for rows finished >1h ago (keeps metadata); `ListDeploymentsByApplication` returns last 50.
- All `timestamptz` map to Go `time.Time`. sqlc config (`sqlc.yaml`): schema `internal/database/migrations`, queries `internal/database/queries`, package `db`, out `internal/database/gen`, `pgx/v5`, `emit_interface`/`emit_json_tags` on.

## 7. Dev workflow in this repo

Brainstorm → spec (`docs/superpowers/specs/`, date-prefixed) → plan (`docs/superpowers/plans/`, date-prefixed) → subagent-driven execution.

- Subagents run STRICTLY SEQUENTIALLY (parallel execution reordered/duplicated tool calls — see Gotchas).
- Subagents do NOT commit on master; the harness blocks it. Only the controller commits.
- Always verify with `make test` before committing.

## 8. Gotchas / lessons (don't repeat these)

- **Parallel subagents reorder/duplicate tool calls** — run subagents sequentially.
- **Traefik API mismatch → routing breaks** when not pinned to ≥v3.6.1 against Docker Engine 29.x (see §3).
- **`StatusBadge` (and any templ component) rendered as literal text** when placed inline after text on the same line — give it its own node.
- **Modals pinned top-left** because Tailwind preflight zeroes `<dialog>` margins — use `.k-modal` (`margin: auto`).
- **CHECK-constraint violations in tests** when omitting `SourceType` (must be `image` or `dockerfile`) or supplying an out-of-set `status`/`trigger`/`role`. Always set CHECK-constrained columns to valid values in fixtures.
- **Tests false-fail with a socket-mount error** off the Colima env — always use `make test` (see §2).

## 9. Where to look next

- [`ROADMAP.md`](./ROADMAP.md) and [`PLAN.md`](./PLAN.md) — phase plan and current focus.
- [`docs/superpowers/specs/`](./docs/superpowers/specs/) and [`docs/superpowers/plans/`](./docs/superpowers/plans/) — date-prefixed design + implementation docs (latest: Phase 3 managed databases and tech-debt cleanup, both 2026-06-02).
- **Phase 5 (domains/TLS/Let's Encrypt) is what the bot needs** (HTTPS webhook URL). Phase 4 (S3 backups) is the immediate next sequential item.
- [`scripts/e2e.sh`](./scripts/e2e.sh) — the authoritative end-to-end scenario covering every major feature; read it to understand expected behavior.

## 10. Docker & CI

- **`Dockerfile`** — multi-stage: `golang:1.26.1-alpine` builder → `CGO_ENABLED=0` static binary → `gcr.io/distroless/static-debian12:nonroot` (~25 MB, runs as UID 65532). It relies on the COMMITTED generated files (no `make generate` in-image); migrations (`//go:embed migrations/*.sql`) and static assets (`//go:embed all:static`) are embedded, so the final image is binary-only. Build locally: `docker build -t krill:local .`.
- **`.github/workflows/ci.yml`** — push (all branches) + PR: build/vet/test (testcontainers works on the runner's native Docker — do NOT add the Colima socket override here), a no-push Docker build, and a generated-drift check (`make generate` + `git diff --exit-code`).
- **`.github/workflows/release.yml`** — `v*.*.*` tag (or `workflow_dispatch`): multi-arch buildx → push to `ghcr.io/${{ github.repository_owner }}/krill` → GitHub Release on tag pushes. Uses `GITHUB_TOKEN`; needs `permissions: packages: write` + `contents: write`. First GHCR push is private — make the package public if anonymous pulls are wanted.
- After changing the build/embed setup, re-verify by actually building the image (`docker build .`) and confirming it still runs (`docker run --rm krill:local` should fail fast on the missing `KRILL_DATABASE_URL`).
