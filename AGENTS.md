# AGENTS.md — Krill

Guidance for AI coding agents and developers working on Krill. Read it before touching code:
it exists to keep you from breaking things or wasting time.

This file holds the rules that apply everywhere. The detail of each feature (invariants, file
and function names, why it is built the way it is) lives in [`docs/internals/`](docs/internals/README.md).
**Before changing a feature, read its internals file** (table in [Features](#features)), and
update that file in the same change when you alter behavior it describes.

> **Design notes are local-only.** Paths under `docs/superpowers/` cited in the internals notes
> are the maintainer's date-prefixed design specs and plans (plus the original Russian
> `ROADMAP.ru.md`/`PLAN.ru.md`). They are git-ignored and not in the public repository — when a
> cited file is missing, the internals notes are authoritative. Never `git add -f` them.

## What Krill is

Krill is a minimal self-hosted PaaS written in Go (`github.com/proshik/krill`) — a lightweight
Dokploy alternative aimed at a ~20–50 MB control plane (vs Dokploy's ~760 MB), built so a webhook
Telegram bot can be hosted on a 2 vCPU / 1.9 GB VPS. It deploys containerized apps and managed
databases on Docker Swarm (one node, or a control plane plus SSH-joined workers), routes traffic
via Traefik, and exposes a web control plane (templ + HTMX + plain CSS on Tailwind v4's preflight)
with a multi-tenant org → project → environment → app/db hierarchy, bcrypt/cookie auth, and an
MCP + REST agent API.

**Status:** the core platform, deploy parity with Dokploy for a real app, managed databases,
backups, multi-server, auto-deploy, notifications, monitoring, self-update and Krill state
backup are done. Several features are done in code but still wait for live acceptance on a real
VPS or cluster. [`ROADMAP.md`](ROADMAP.md) is the source for what is done, what is open, and the
live-acceptance queue; the historical status trail is in
[`docs/internals/README.md`](docs/internals/README.md#feature-status-trail).

## Build, run, test

**Tests need the Colima testcontainers env.** DB/integration tests start testcontainers, and
Ryuk (testcontainers cleanup) needs BOTH of these on Colima's virtiofs:

```
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock        # host socket path
TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock  # in-VM socket path
```

`make test` sets both. Without them tests false-fail with a socket-mount error. NEVER report
"Docker not running" without first trying this env. Without `make`:

```
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock \
TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock \
go test ./...
```

**After editing any `.templ` or `.sql` file run `make generate`.** Generated code
(`*_templ.go`, `internal/database/gen/*.sql.go`, `internal/web/static/app.css`) is committed and
the build uses it; CI fails on drift.

| Task | Command |
|------|---------|
| Generate code (templ + sqlc + css) | `make generate` |
| Build binary | `make build` (→ `bin/krill`) |
| Run server | `make run` |
| Rebuild CSS (minified) / watch | `make css` / `make css-watch` |
| Run all tests (with Colima fix) | `make test` |
| Integration tests only | `make test-integration` (`-tags=integration`) |
| Lint new migrations for rollback compatibility (squawk) | `make lint-migrations` |
| Start / stop dev Postgres | `make db-up` / `make db-down` |
| `go mod tidy` | `make tidy` |
| Download Tailwind binary (first time) | `./tools/get-tailwind.sh` |
| Download squawk binary (first time, checksum-verified) | `./tools/get-squawk.sh` |
| Full E2E smoke test | `./scripts/e2e.sh` |

A few tests use other build tags or env: `-tags=realsystemd` (the firewall confirm protocol in a
privileged systemd container) and `KRILL_IT_CONSTRAINT=node.role==worker` (a log test against a
multi-node Swarm); see each test's comment.

- **Local ports:** app on `:18080`, Postgres on `55432` (the `.env` config used when `8080`/`5432`
  are busy; `.env.example` uses `8080`/`5432`). Login: `admin@krill.local` / `changeme`.
- **Prereqs:** Go 1.26.11, Docker/Colima running, Swarm initialized (`docker swarm init`),
  Tailwind v4.3.0 standalone binary at `./tools/tailwindcss` (no Node/npm).
- **Startup order** (`cmd/krill/main.go`): open the `KRILL_LISTEN_ADDR` listener behind a
  `server.StartupGate` (a "Krill is starting" page) → run embedded migrations → seed the admin user
  and default org → reconcile Traefik onto the base network plus every organization's network →
  swap in the real router. The per-organization network migration runs afterwards, in the
  background. `krill --version` (`-v`) is handled before `config.Load()` and needs no environment;
  the self-updater smoke-tests downloaded binaries with it.
- **Configuration:** required `KRILL_DATABASE_URL`, `KRILL_ADMIN_EMAIL`, `KRILL_ADMIN_PASSWORD`.
  Every variable, with its default, is in [`docs/configuration.md`](docs/configuration.md) — the
  only full list; add a new variable there. Traps: `KRILL_COOKIE_SECURE=true` breaks sign-in over
  plain `http://ip:8080` (the panel domain gets `Secure` cookies per request without it);
  `KRILL_TRUST_PROXY` only behind an external proxy that sets `X-Forwarded-For`, since clients can
  forge it; `KRILL_AGENT_API_ENABLED` gates BOTH `/mcp` and `/api/v1` (it was `KRILL_MCP_ENABLED`
  until 2026-08-13); an empty `KRILL_SECRET_KEY` stores secrets in plaintext with a startup warning.

## Features

Before changing a feature, read the internals file in the right column.

| Feature | In one line | Read |
|---------|-------------|------|
| Per-organization networks | each org gets overlay `krill-org-<id>`; a background `orgnet.Migrator` moves existing services (DBs before apps) and marks an org only when every step finished | [networking.md](docs/internals/networking.md#per-organization-networks) |
| Panel domain | the UI behind Traefik through its HTTP provider (`/_krill/gateway/config`), two HMAC tokens, anti-lockout confirm, optional direct-port close, HSTS | [networking.md](docs/internals/networking.md#panel-domain) |
| Domains, TLS, exposure, protection | `domains` drives Traefik labels; new apps are internal by default; per-domain paths, basic-auth, IP allowlist | [networking.md](docs/internals/networking.md#domainstls) |
| Shared hosts, redirects, raw ports | apps of one env share a host by path under a per-host advisory lock; redirect to the primary domain; host-mode TCP/UDP ports | [networking.md](docs/internals/networking.md#shared-hosts-by-path) |
| Deploy pipeline | queue + one worker, baseline-relative convergence, background settle, Stop cancels unapplied deploys, typed refusals | [deploy.md](docs/internals/deploy.md) |
| App settings, registries, git, auto-deploy, volumes | advanced container settings, resource defaults and build caps, private pull, credential host binding, build secrets, GitHub webhook and deploy hook, volume backup | [deploy.md](docs/internals/deploy.md) |
| Managed databases | org-level `db_instances` + env-scoped `logical_databases`; a `Driver` per engine (postgres/redis/dragonfly/valkey/minio); stop-first + flock single-writer guard | [databases.md](docs/internals/databases.md) |
| DB links, buckets, backups | connection-string injection, S3 buckets managed with `mc` inside the instance, pg_dump/volume/bucket backups to S3 | [databases.md](docs/internals/databases.md) |
| Cluster, topology, systems, monitoring | SSH worker join and placement, node-aware exec, topology map, environment canvas, cluster-wide stats | [cluster.md](docs/internals/cluster.md) |
| Self-update, state backup | install a release from Settings → Updates with dead-man rollback; age-encrypted `pg_dump` of Krill's own DB to S3 | [operations.md](docs/internals/operations.md) |
| Observability, metrics, notifications, logs, terminal | Alloy node agent and app collector, Telegram alerts and digest, structured log viewer, web terminal | [operations.md](docs/internals/operations.md) |
| Agent API | twelve operations plus four provisioning ones over `/api/v1` and `/mcp`, org-scoped bearer tokens | [agent-api.md](docs/internals/agent-api.md) |
| Auth and secrets | tenancy chain, RBAC, instance operator, password changes, `secret.Enc`/`Dec` | [security.md](docs/internals/security.md) |
| Web UI | page frame, CSS, i18n, `app.js` helpers | [ui.md](docs/internals/ui.md) |
| Packages, data model, migrations | detailed package map, data-model facts, migration policy | [code-map.md](docs/internals/code-map.md) |

## Rules (follow these)

### Repository and code

- **Commit messages:** conventional commits (`feat(scope): ...`, `fix(scope): ...`, `test(...)`,
  `docs(...)`). HARD RULE: no `Co-Authored-By` / AI attribution trailers of any kind.
- **Code is English-only** — comments, `slog` messages, `fmt.Errorf`/`errors.New` strings,
  `http.Error` messages. The only Russian in the repo is the `ru` UI catalog
  (`internal/web/i18n/ru.go`). No Russian in `.go`, `.templ`, `.sql`, `.sh`, `.css` or config.
- **The repository is public:** use neutral stand-ins for hostnames, IPs and emails in fixtures
  and docs.
- **Handlers never swallow errors** — `slog.Error("context", "err", err)` or a 500. No silent
  ignores in delete/deploy/version handlers.
- **Logging:** structured `slog`; in handlers use `logFrom(r)` (tagged `request_id`/`user_id`/
  `org_id`) for error logs before a 5xx and for INFO audit lines before a success redirect. **Never
  log secrets** — passwords, temp passwords, tokens, connection strings, env values. See
  [operations.md](docs/internals/operations.md#logging).
- **Unique violations:** detect with `errors.As(*pgconn.PgError)` + code `23505` (`mapUniqueErr`),
  never by string matching.
- **Ports from user input** are parsed (`strconv.Atoi`), range-checked `1..65535` (400 on bad
  input) and conflict-checked across `app_ports` and DB `external_port`.

### UI

- **All UI strings go through `i18n.T(ctx, "key")` / `i18n.Tf`**, with keys in both the `en` and
  `ru` catalogs (kept in sync). No raw user-facing literals in templates or handlers.
- **UI text is lowercase in the catalogs themselves** (headings too); proper names and identifiers
  keep their case. Use `i18n.Sentence` where Go joins messages into sentences, or for a tab title
  that starts with a page name. Telegram text (`tg.*` keys, `internal/notify`) keeps sentence case.
- **Destructive forms** carry `hx-boost="false"` + `data-confirm` and call `krillConfirm(this)` —
  native `confirm()` is broken under hx-boost. `app.js` loads ONCE in the layout `<head>`; never add
  per-tab `app.js` or inline mount calls (`krillEnhance` mounts terminals and editors).
- **Double submission is guarded globally** (`krillGuardForms`: `hx-sync="this:drop"` +
  `hx-disabled-elt`). A handler whose duplicate is harmful still needs a server-side guard; toggles
  post the desired state (`enabled=1`/`0`) instead of flipping.
- **Cards with several sibling forms** that could drop each other's unsaved input get
  `data-unsaved-scope` (as the domain card does).
- Details and traps: [ui.md](docs/internals/ui.md).

### Tenancy and permissions

- **Every app/db route chain-checks tenant ownership and 404s on mismatch** — `loadOrg` →
  `loadProject` → `loadEnvironment` → `loadApp`/`loadDBChain`, each FK checked against its parent,
  `http.NotFound` on mismatch. IDOR regression tests exist; keep them green. Cross-org is 404,
  never 403.
- **RBAC is owner > admin > member** (`Role`, `AtLeast`, `RequireRole(min)` → 403). Admin+ manages
  projects/apps/dbs/members; members are read-only. App POST routes and DB backup download/objects
  sit in a `RequireRole(RoleAdmin)` group; plaintext connection strings are admin-only. Only an
  owner may grant owner (`updateMemberRole`).
- **Global infrastructure is gated by `RequireInstanceAdmin`, never `RequireRole`.** Org admin is
  self-grantable (anyone can `POST /orgs` and own the new org). Nodes, monitoring, panel domain,
  updates, observability and Krill backups use the instance flag `users.is_admin` (only the seeded
  `KRILL_ADMIN_EMAIL` user, promoted by `SeedAdmin`; loaded by `WithInstanceAdmin`); non-operators
  get 404. Node data in notifications is operator-only too.
- **Login:** unknown emails cost a dummy bcrypt (`auth.dummyHash`); `POST /login` is rate-limited
  per source IP (10/min). Temp passwords travel in the HttpOnly `krill_flash_pw` cookie, never the
  URL.
- Details: [security.md](docs/internals/security.md).

### Secrets

- Encrypt at the store boundary with `secret.Enc`, read with `secret.Dec`.
- **`secret.Dec` fails loudly:** it returns `("", secret.ErrUndecryptable)` for an `enc:v1:` value
  it cannot open (missing/rotated key). Operations that need the credential fail; display paths log
  and render nothing. Never reintroduce `, _ :=` at a `secret.Dec` call.
- Never put a secret in a container's Env, argv or labels when it can go over stdin — `docker
  inspect` and the daemon's debug log show them (see the state-dump sidecar in
  [operations.md](docs/internals/operations.md#state-dump-sidecars-krillrolestate-dump-get-the-dsn-password-on-stdin-never-in-env-argv-or-labels)).
  `Engine.Exec`'s error text embeds the whole argv, so never wrap it when argv carries a secret.
- A new table holding a live bearer credential in plain form (like `sessions`) must be added to
  `statebackup.DumpArgs()`'s `--exclude-table-data` list, or every state backup carries working
  credentials.
- The agent API never returns env values (`krill_list_env` gives names and source only): whatever
  an agent reads reaches its model provider.

### Deploys

Expanded in [deploy.md](docs/internals/deploy.md#deploy-invariants).

- **Convergence is baseline-relative:** snapshot the service's task IDs before `ServiceDeploy`,
  then count only new tasks (`ServiceProgress(name, baseline)`); counting all tasks declares every
  StartFirst redeploy converged on the old task. A `rollback_*` `UpdateState` fails the deploy.
- **A deploy is done only when Swarm is:** after `KRILL_CONVERGE_TIMEOUT` a still-converging
  service goes to `Deployer.startSettle` (up to 20m, in `d.wg`), which records the real outcome;
  the deployment stays `running`, so the per-app and per-org caps and the self-update busy check
  still count it. A shutdown records "interrupted" and leaves the app `deploying`;
  `deploy.StartStatusHeal` later sets such a status from the live service.
- **Stop cancels an unfinished deploy:** both Stops (UI and API) go through `Deployer.StopApp`,
  which cancels queued/preparing deploys under `admitMu` and scales under `oplock.AppRouting` — the
  lock the deployer holds from `enterApply` through `ServiceDeploy` — so exactly one of them wins.
- **Deploy jobs carry their tag:** pass a new image/tag in the job (`Deployer.EnqueueImage`); never
  retag `applications` before enqueueing — a refused deploy would leave the tag behind.
- **Refusals are typed:** `ErrDeployInFlight`, `ErrOrgDeployLimit`, `ErrQueueFull`,
  `ErrDeployerStopped`; a full queue is refused before a deployment row exists. UI flashes say which
  (`deployRefusalKey`); the API maps in-flight to 409 and the rest to 503 + `Retry-After`; webhooks
  answer 503 + `Retry-After`, never a silent 202.
- **New optional `ServiceSpec` fields get `json:",omitempty"`:** the gateway and observability
  agents fingerprint the serialized spec, so a field that serializes when unset redeploys them on
  upgrade.
- **Build logs are bounded** (128 KB head + 128 KB tail) and made storable (`storable`: no cut
  multi-byte characters, no NUL) before any write — PostgreSQL otherwise refuses the whole update.
- **A successful deploy records what it applied** (`deployments.applied_config`, per-section
  HMAC digests): stored and current fingerprints come from one function,
  `Deployer.configFingerprint` over `configSections` — add a new deploy input there, never in a
  second list. "Apply settings" redeploys the running content-addressed image with the current
  settings (no tag pull, no build). See
  [deploy.md](docs/internals/deploy.md#saved-settings-that-are-not-live-yet-and-apply-settings).
- **A service with a host-mode published port deploys StopFirst** (`buildSwarmSpec`): StartFirst
  deadlocks on the port without any error. A change of `UpdateConfig.Order` applies in the very
  rollout that makes it.

### Managed databases

- Every DB driver's spec sets `UpdateStopFirst: true`, and postgres/redis/dragonfly/valkey wrap the
  entrypoint in a portable `sh -c 'exec 9<"$0" && flock 9 && exec …'` on the mount directory — two
  postmasters on one `PGDATA` would silently lose writes. Never `flock --no-fork` (busybox lacks
  it). MinIO's image has no `flock` (stop-first only). Start/Stop only scale: an instance is
  protected after its next Deploy, version change or node move.
- Acting paths look engines up with `drivers.Registry.Lookup` (error), never `MustGet` (nil →
  panic in the deploy goroutine).
- `ProvisionLogicalDB` checks `pg_database`/`pg_roles` first and returns `ErrObjectsExist` without
  touching anything; compensation must never drop a database Krill did not create.
- The `<appName>-data` volume survives instance delete unless `destroy_data=on`.
- Details: [databases.md](docs/internals/databases.md).

### Migrations (expand/contract)

- **A migration must keep the previous release bootable**, because self-update rolls back to it.
  Additive only: new tables, nullable or defaulted columns, non-unique indexes. Not additive: new
  unique indexes or FKs on existing tables, dropping an index used by `ON CONFLICT`,
  `DROP`/`RENAME`/type changes/`SET NOT NULL`/`DROP NOT NULL`/`NOT NULL` without a default (safe
  only one release after the code stopped using the old shape). A new `CHECK` must not reject
  anything the previous release writes; widen reads a release before widening writes.
- **squawk** (`make lint-migrations`, in CI, for files above `MIGRATION_COMPAT_FROM`) catches the
  mechanical cases only — reviewers still read every migration (CHECKs, enums, `CREATE UNIQUE
  INDEX` on existing tables). Intentional break: `-- squawk-ignore <rule> -- <reason, safe from
  vX>` on the line directly before the reported line.
- A failed or interrupted migration leaves the schema dirty and defeats the rollback; migrations
  run under their own signal context (`RunMigrationsContext`), so SIGTERM stops between migrations.
- Deleting a `.sql` query file: also `git rm` its `internal/database/gen/<name>.sql.go` — sqlc
  never prunes it, and the stale file still compiles.
- Full policy and recovery: [code-map.md](docs/internals/code-map.md#migrations).

## Pitfalls that bite

Short form; each links to the full note.

- **Traefik is pinned to `v3.6.1`** (`traefik.TraefikVersion`): ≤3.6.0 hardcodes Docker API 1.24,
  which Docker 29.x rejects — all routing breaks. Configured by CLI args only; the only mount is the
  docker socket (ro). [networking](docs/internals/networking.md#traefik-is-pinned-to-v361)
- **Gateway timeouts are pinned explicitly** (`traefik.gatewayTimeoutArgs`); any arg change alters
  the spec fingerprint, so the first start after an upgrade redeploys the gateway once.
  [networking](docs/internals/networking.md#gateway-timeouts-are-explicit-and-long-responses-are-safe)
- **Shared hosts need explicit router priorities** (path 200, catch-all 100, panel 1,000,000);
  `/api` matches `Path(/api)` or `PathPrefix(/api/)`, never a raw prefix; hidden metrics paths use a
  case-insensitive `PathRegexp` that also covers `;` parameters.
  [networking](docs/internals/networking.md#shared-hosts-need-explicit-priorities)
- **`KRILL_OVERLAY_MTU` is the container MTU**; the network option is the underlay MTU (+50), set at
  creation only. [networking](docs/internals/networking.md#an-overlay-networks-comdockernetworkdrivermtu-is-the-underlay-mtu-not-the-containers)
- **Firewall dead-man switch: one snapshot per node.** A second Apply before Confirm is refused;
  firewall handlers hold `Server.fwMu`; Confirm and the revert share an `flock` protocol.
  [networking](docs/internals/networking.md#the-firewall-dead-man-switch-has-one-snapshot-per-node)
- **Remote docker clients: `client.WithHost` BEFORE `client.WithDialContext`** in
  `NewRemoteClient`/`NewRemoteStats` — otherwise the SSH tunnel dialer is clobbered and the
  "remote" client talks to the local daemon. Exec, terminal and provisioning are node-aware through
  `RemoteClientProvider`. [cluster](docs/internals/cluster.md#exec-operations-logical-db-provisioning-pg_dump-backups-restore-web-terminal-are-node-aware)
- **A non-following `docker service logs` over several tasks can stall** (swarmkit sends on one
  gRPC stream from several goroutines): read tasks one by one (`TaskLogs`, `taskLogTimeout`) and
  bound every backlog read (`logparse.BacklogTimeout`).
  [operations](docs/internals/operations.md#a-non-following-service-log-read-can-stall)
- **A volume restore replaces the volume's contents** (it clears the mount point first); callers
  quiesce the app first. [deploy](docs/internals/deploy.md#a-volume-restore-replaces-the-volumes-contents)
- **Deleting an environment or project** removes its apps' services and volumes, drops its logical
  databases and the bucket users Krill generated; servers, their volumes and bucket contents stay.
  Keep `envset.delete_hint`/`confirm.delete_project_more` in step with the handlers.
  [databases](docs/internals/databases.md#named-volume-appname-data-persists)
- **DB log feeds use negative feed IDs** (`InstanceFeedID(id) = -id`) so they never collide with
  deployment IDs in `DeployLogHub`. [databases](docs/internals/databases.md#db-log-feeds-use-negative-feed-ids-to-avoid-colliding-with-positive-deployment-ids)
- **golang-migrate's `Up()` fails on a newer schema**; `RunMigrations` skips `Up()` with a warning
  when the recorded version is ahead — that is what makes rollback possible.
  [code-map](docs/internals/code-map.md#golang-migrates-up-fails-on-a-schema-newer-than-the-embedded-migrations)
- **Shutdown:** a long-lived non-WebSocket response (MCP `GET /mcp`) holds `http.Server.Shutdown`;
  mount any new streaming GET behind `endStreamOnShutdown`. A stop past `shutdownBudget` (10s) logs
  the in-flight requests and still exits 0.
  [operations](docs/internals/operations.md#a-long-lived-non-websocket-response-holds-httpservershutdown-until-its-deadline)
- **Never restart `krill.service` from inside its own cgroup** — the stop SIGTERMs the caller; use a
  transient `systemd-run` unit (`Updater.restartCmd`). Units set `StartLimitIntervalSec=0` so a
  fast-failing unit keeps retrying instead of parking in `failed`.
  [operations](docs/internals/operations.md#never-restart-krillservice-from-inside-its-own-cgroup)
- **Self-update ordering:** the pending marker is written before the binary rename, and the revert
  script exits when the marker is gone. A `git describe` version is a semver prerelease — only a
  plain `vX.Y.Z` (`buildinfo.IsRelease`) is offered updates.
  [operations](docs/internals/operations.md#the-pending-self-update-marker-is-written-before-the-binary-is-renamed-into-place-not-after)
- **`alloy validate` proves little** (it misses expression errors and bad string escapes); only an
  integration test running the real agent catches them. Alloy `body_size_limit` must be a unit
  string (`"5MiB"`); `poll_timeout` stays strictly below `poll_frequency`, a scrape timeout
  never above its interval.
  [operations](docs/internals/operations.md#alloy-validate-does-not-catch-expression-errors)
- **Tailwind supplies only its preflight**; no utility classes are generated. Its `* { margin: 0 }`
  un-centres `<dialog>` — a new modal dialog needs `margin: auto` like `dialog.confirm`. Assets are
  linked with `?v=web.AssetVersion`. [ui](docs/internals/ui.md#tailwind-supplies-only-its-preflight)
- **templ:** a `@Component(...)` inline after text renders as literal text — put it on its own
  node (`StatusBadge` bit us). A literal `@` between two `{ }` expressions does not parse — build
  one Go string (`{ a + " @ " + b }`). [ui](docs/internals/ui.md#templ-component-on-its-own-line-gotcha)
- **`.term` needs an explicit CSS height** or FitAddon computes 0 rows on a boosted swap.
  [ui](docs/internals/ui.md#confirm-dialogs--log-terminals-appjs-loads-once-in-the-layout-head-deferred-not-per-tab)
- **CHECK-constraint violations in test fixtures:** always set `SourceType` (`image`/`dockerfile`)
  and valid `status`/`trigger`/`role` values.
- **Subagents running in parallel reorder/duplicate tool calls** — run them strictly sequentially.

## Architecture map

A single Go binary (HTTP on `:8080`/`:18080`) keeps all state in PostgreSQL (pgxpool + sqlc) and
drives Docker Swarm through the Docker API. Traefik (a Swarm service on host `:80`/`:443`) routes
by `Host` to app/db services, each on its organization's overlay network (`krill-org-<id>`;
Traefik joins all of them). One deployment worker goroutine serializes build/deploy jobs. Full
package detail: [code-map.md](docs/internals/code-map.md#packages).

- `cmd/krill` — entry point and wiring (config → migrate → pool → seed → services → Traefik → chi
  server → graceful shutdown).
- `internal/config` — env parsing (caarlos0/env).
- `internal/server` — chi router, middleware, all handlers; tenancy loaders in `context.go`
  (`loadDBChain` in `db_handlers.go`).
- `internal/auth` — sessions (`krill_session`, 7 days), bcrypt, RBAC, `RequireAuth`/
  `RequireOrgMember`/`RequireRole`/`RequireInstanceAdmin`.
- `internal/org` — org/project/env/app/member CRUD.
- `internal/docker` — Swarm API wrapper (`Engine`); optional capabilities by type assertion
  (`SwarmObjects`, remote exec). Rolling updates with parallelism 1, StartFirst (StopFirst with
  host-mode ports), rollback on failure; DNSRR for DBs, VIP for apps.
- `internal/deploy` — `Deployer`, `DeployLogHub`, store, statuses; `internal/builder` — git clone +
  `docker build`, build-cache prune.
- `internal/dbservice` (+ leaf `drivers`) — DB instances, logical DBs, buckets; `drivers` imports
  only docker + secret, so `deploy` can import it without a cycle.
- `internal/traefik`, `internal/panel`, `internal/orgnet`, `internal/hostshare`,
  `internal/firewall` — gateway, panel domain, org networks, shared hosts, nftables.
- `internal/backup`, `internal/volume`, `internal/bucketbackup`, `internal/statebackup` — backups.
- `internal/cluster`, `internal/metrics`, `internal/topology` — nodes, sampling, pure graph builders.
- `internal/selfupdate`, `internal/buildinfo`, `internal/observability`, `internal/appmetrics`,
  `internal/notify` — operations.
- `internal/api` (transport-agnostic service layer + identity ctx key) and `internal/mcpsrv` (MCP
  adapter; must not import `internal/server`).
- `internal/web` — embedded static, i18n, templ templates; `internal/logparse` — log-line parser;
  `internal/secret`, `internal/netguard` (SSRF egress guard), `internal/webhook`, `internal/oplock`.
- `internal/database` — embedded migrations (`RunMigrationsContext`), sqlc `gen/` (generated, do
  not edit); `internal/testutil` — `NewTestDB(t)` and other testcontainers.

## Data model

Key facts only; tables and more detail are in [code-map.md](docs/internals/code-map.md#data-model),
the migrations themselves in `internal/database/migrations`.

- **Tenancy chain** (FK, ON DELETE CASCADE): `organizations` → `projects` → `environments` →
  `applications`. `db_instances` hang off `organizations`; `logical_databases` reference their
  instance (RESTRICT) and environment (CASCADE). `members` is `UNIQUE(org_id,user_id)` with a role
  CHECK (`owner`/`admin`/`member`). `cluster_nodes` and the singletons (`panel_gateway`,
  `observability_settings`, `state_backup_settings`) are global, not org-scoped.
- `db_instances.app_name` is UNIQUE and is the Swarm service name = DNS hostname = volume prefix
  (`<appName>-data`); generated once, never changes.
- `applications.env_text` is the SINGLE source of env vars — raw, order-preserving `KEY=VALUE`
  lines; the deploy map is derived at runtime (`deploy.parseEnvText`); duplicate keys are rejected.
- `domains` is the routing source of truth (`exposed` default false, `paths`, protection,
  `redirect_to_primary`); `applications.domain` is the denormalized primary and stays UNIQUE, while
  host sharing is enforced by `internal/hostshare` under a per-host advisory lock.
- `deployments` is append-only history (`status` `running`/`done`/`error`; `trigger`
  `manual`/`webhook`/`schedule`/`api`/`system`); old logs are cleared, metadata kept.
- CHECK-constrained columns (`source_type`, statuses, triggers, roles, engines, link fields) need
  valid values in fixtures. Nullable columns are pointers (sqlc `emit_pointers_for_null_types`).

## Dev workflow

- Brainstorm → spec (`docs/superpowers/specs/`, date-prefixed) → plan (`docs/superpowers/plans/`)
  → subagent-driven execution. Both directories are local-only (see the note at the top).
- Subagents run STRICTLY SEQUENTIALLY and do not commit on master; only the controller commits.
- Always verify with `make test` before committing.
- **Docs layout:** `README.md` is a short landing page (badges, screenshots from `docs/images/`,
  features, quick start, status, docs index) — keep detail in `docs/`. User and operator docs:
  `docs/install.md` (install, upgrade, external Postgres, rollback, uninstall),
  `docs/configuration.md` (every `KRILL_*` variable), `docs/guides/`, `docs/architecture.md`;
  contributor docs: `docs/development.md`, `CONTRIBUTING.md`; implementation notes:
  `docs/internals/`. Anything a contributor needs belongs in these tracked files.

## Docker, CI, release

Full notes: [operations.md](docs/internals/operations.md#release-image-and-installer).

- **`Dockerfile`:** `golang:1.26.11-alpine` → static `CGO_ENABLED=0` binary →
  `gcr.io/distroless/static-debian12:nonroot`. It relies on the COMMITTED generated files;
  migrations and static assets are embedded. After changing the build/embed setup, build the image
  and check that `docker run --rm krill:local` fails fast on the missing `KRILL_DATABASE_URL`.
- **`ci.yml`:** build/vet/test (native Docker on the runner — do NOT add the Colima socket
  override), a no-push Docker build, the generated-drift check, migration lint.
- **`release.yml`:** on a `v*.*.*` tag, a multi-arch image to `ghcr.io/<owner>/krill`, static
  `krill-linux-amd64`/`-arm64` + `checksums.txt` on the GitHub Release; both stamped via
  `-X github.com/proshik/krill/internal/buildinfo.Version=<tag>` / `VERSION=<tag>`.
- **`site/` + `pages.yml`:** the landing page — Hugo + the Hextra theme as a Hugo module pinned in
  `site/go.mod` (a nested module the root `go ./...` ignores), Hugo pinned by `tools/get-hugo.sh`,
  `make site`/`make site-serve`; deployed to GitHub Pages on pushes to master that touch it, and
  `ci.yml` skips site-only changes. Landing only: docs links point at GitHub, claims must come
  from the README and docs. See [Landing site](docs/development.md#landing-site).
- **`install.sh`:** host binary + systemd (not a container), idempotent (re-run = upgrade),
  checksum-verified and fail-closed (`KRILL_SKIP_VERIFY=1` overrides), loopback-only
  `krill-postgres` on its own `krill-state` network, `/etc/krill/krill.env` generated once.

## Where to look next

- [`ROADMAP.md`](ROADMAP.md) — what is done, what is open, the live-acceptance queue.
- [`docs/internals/`](docs/internals/README.md) — per-feature implementation notes.
- [`scripts/e2e.sh`](scripts/e2e.sh) — the authoritative end-to-end scenario.
- [`skills/krill-deploy/SKILL.md`](skills/krill-deploy/SKILL.md) — agent-side runbooks for the
  agent API.
