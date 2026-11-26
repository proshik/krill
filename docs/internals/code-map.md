# Code map, data model and migrations

The detailed package map, the data model's key facts, and the schema migration policy.

## Runtime topology

Runtime topology: a single Go binary (HTTP on `:8080`/`:18080`) holds all state in PostgreSQL (pgxpool) and drives a single-node Docker Swarm via the Docker API. Traefik (a Swarm service on host `:80`) watches the Swarm API and routes by `Host` header to app/db services, each on its own organization's overlay network (`krill-org-<id>`; Traefik is attached to all of them) — see [Per-organization networks](networking.md#per-organization-networks). One deployment worker goroutine serializes all build/deploy jobs.

## Packages

- **`cmd/krill`** — entry point: load config → migrate → pgxpool + sqlc → seed admin/default org → wire services → bootstrap Traefik+network → start chi server → graceful shutdown.
- **`internal/config`** — env parsing via caarlos0/env (`Config`, `Load()`).
- **`internal/server`** — chi router, middleware chain, all handlers; path-scoped multi-tenant routes. Tenancy chain-checks `loadOrg`/`loadProject`/`loadEnvironment`/`loadApp` live in `context.go`; `loadDBChain` lives in `db_handlers.go`.
- **`internal/auth`** — sessions, bcrypt, RBAC (`Role` member<admin<owner, `AtLeast`), cookie `krill_session` (`CookieName`, `SessionTTL` = 7 days); middleware `RequireAuth`, `RequireOrgMember`, `RequireRole`, `WithInstanceAdmin`/`RequireInstanceAdmin` (instance-operator gate for global infra, backed by `users.is_admin`); ctx helpers `UserID`/`OrgID`/`RoleOf`/`IsInstanceAdmin`.
- **`internal/org`** — org/project/env/app/member CRUD, `Slugify`, `ErrNotFound`/`ErrSlugTaken`, `mapUniqueErr`.
- **`internal/docker`** — Swarm API wrapper. **`Engine` interface methods:** `NetworkEnsure(ctx,name)→(created bool,err error)` (the `created` flag lets a caller like `traefik.Reconcile` tell "the network is new" apart from "the name is unchanged", since Swarm doesn't expose network identity by name), `NetworkRemove(ctx,name)` (idempotent, modeled on `NetworkEnsure`; no production caller yet — there is no organization-delete endpoint), `ServiceDeploy(ctx,ServiceSpec)` (`ServiceSpec.Networks []string`, when set, supersedes the single `Network` field — only a service attached to more than one network, today just the Traefik gateway, sets it), `ServiceRemove(ctx,name)`, `ServiceState(ctx,name)→ServiceState`, `ServiceStates(ctx,names)` (bulk, used to filter out stopped/undeployed apps and to park stopped DB instances before the org-network migration redeploys them), `ServiceProgress(ctx,name,excludeTaskIDs)→ServiceProgress` (baseline-relative convergence: new-task Running/Failed + swarm `UpdateState` for rollback detection), `ServiceLogs(ctx,name,follow)→io.ReadCloser`, `ServiceLabels(ctx,name)→(map[string]string,bool,error)`, `ServiceScale(ctx,name,replicas uint64)`, `ImagePull(ctx,ref,out)`, `VolumeRemove(ctx,name)`. Helpers: `ServiceName(appID)→"krill-<id>"`, `BuildImageTag(appID,deployID)→"krill-<id>:<deployID>"`. Rolling update is parallelism=1, StartFirst (zero-downtime) — except for a service publishing a host-mode port, which is StopFirst (see [the host-mode port pitfall](networking.md#a-host-mode-published-port-deadlocks-a-startfirst-update)) — rollback-on-failure; DNSRR for DBs, VIP for apps. **`docker.SwarmObjects`** is the optional capability to manage Swarm configs/secrets (`ConfigEnsure`/`SecretEnsure`/`PruneObjects`), implemented only by the real `dockerEngine` (callers type-assert, same pattern as `RemoteExecConfigurable`) — used by `internal/observability` to mount the agent's rendered config and target passwords without touching the `Engine` interface itself.
- **`internal/deploy`** — `Deployer` (queue + 1 worker, 10-min job timeout, 90s convergence poll), `DeployLogHub` (in-memory head+tail buffer + WS subscribers), `Store` interface, status values (`idle`/`deploying`/`running`/`error`), `DeriveStatus`, `StartLogCleanup` (truncates logs >1h old).
- **`internal/builder`** — `Builder` interface; `gitBuilder` does shallow `git clone --depth=1` + `docker build`; `ValidateBuildRequest` guards against flag injection and path traversal.
- **`internal/dbservice`** — managed DB-instance lifecycle over Engine+Store (create/deploy/start/stop/version/node/delete, node-pinned volumes, logical-DB provisioning, external-access socat proxies). Per-engine knowledge lives in the leaf subpackage **`internal/dbservice/drivers`** — a `Driver` per engine (`postgres`/`redis`/`dragonfly`/`minio`/`valkey`) in `drivers.Registry` supplying `BuildSpec`/`ExternalTargets`/`LinkFields`/`LinkValue`/`ConnDisplay`/`HasLogicalResource`; imports docker+secret only (no cycle, so `deploy` can import it too). See [Stateful services](databases.md#stateful-services--engine-drivers).
- **`internal/traefik`** — `TraefikVersion`, `TraefikSpec(networks []string, acme)` (first network is the swarm provider's default; the full list becomes `ServiceSpec.Networks`), `Reconcile(ctx,eng,baseNetwork,orgNetworks,acme)` (idempotent — deploys only when a spec-fingerprint label differs from what's running, or a network was freshly created), `AppLabels(...)` generating `traefik.http.routers.*`/`services.*` labels. `Reconcile` replaced the old one-shot `Bootstrap` when Traefik needed to join every organization's network, not just one — see [Per-organization networks](networking.md#per-organization-networks).
- **`internal/panel`** — the UI's own route through the gateway: `DynamicConfig`, `Upstream`, `NormalizeHost`, `DeriveTokens`/`Matches`, `ViaGateway`/`SecureViaGateway`, `CheckCertificate`, `EnsureSecret` — see [Panel domain](networking.md#panel-domain).
- **`internal/orgnet`** — `Name(orgID)→"krill-org-<id>"`; `Migrator` (the background startup job that moves existing organizations onto their own network) and its `DBStore` adapter — see [Per-organization networks](networking.md#per-organization-networks).
- **`internal/buildinfo`** — `Version` (linker-stamped by `release.yml`/`make build` via `-X ...buildinfo.Version=<tag>`, else derived from Go module/VCS build info, else `"dev"`), `String()`, `IsRelease(v)` (a plain `vX.Y.Z` tag, no prerelease suffix).
- **`internal/selfupdate`** — `Checker` (discovers the latest GitHub release) and `Updater` (`Start`/`Rollback`/`Status`/`Previous`/`ConfirmStartup`), plus the marker/drop-in/timer plumbing behind them — see [Self-update](operations.md#self-update).
- **`internal/observability`** — the node observability agent: `Settings`/`Load`/`Save`/`SetEnabled` over `observability_settings`, `RenderNodeConfig` (the Alloy config template), `NodeSpec`/`Reconcile`/`Reconciler` (converges `krill-alloy-node` via `docker.SwarmObjects`), `ContainerLabels` (the `krill.*` labels apps carry for Alloy's log discovery), and `Checker.Check` (the settings-page "Check" button) — see [Observability agent](operations.md#observability-agent).
- **`internal/web`** — embedded static (`Static()` in `embed.go`), i18n (`WithLocale`, `T`, `en` catalog), templ templates (`layout`, `login`, `orgs`, `org_dashboard`, `project`, `app_detail`, `database_detail`, `deployments`, `members`, `status_badge`, plus shared `ui/components`).
- **`internal/database`** — `RunMigrationsContext(ctx, dsn)` / `RunMigrations(dsn)` (embedded, idempotent, ignores `ErrNoChange`; a cancelled ctx stops between migrations, never inside one), sqlc-generated `gen/` (read-only).
- **`internal/topology`** — pure cluster-topology graph builder (`Build`, `DetectEnvLinks`) for the Topology page: nodes/services/DB-link assembly + `env_text`-DSN link detection, no DB/docker access of its own.
- **`internal/api`** — the agent-facing service layer: token generation/hashing (`GenerateToken`/`HashToken`/`TokenMatches`/`PrefixOf`), `Authenticator` (prefix lookup → constant-time hash compare → live membership), `Identity`/`CanWrite`, the identity ctx key (`WithIdentity`/`IdentityFrom`), `Error{Code,Message}`, and the twelve operations (`Whoami`/`ListApps`/`AppStatus`/`AppLogs`/`ListEnv`/`ListDeployments`/`DeploymentStatus`/`Deploy`/`Rebuild`/`Reload`/`Stop`/`SetEnv`). Knows nothing about HTTP or MCP; both adapters call these.
- **`internal/mcpsrv`** — the MCP adapter over `internal/api.Service`: one shared `*mcp.Server` with the twelve tools registered from `toolRegistrations` (the single source `ToolNames()` also reads, so the drift test cannot go stale) behind one streamable-HTTP handler. Reads identity from ctx; contains no tenancy or permission logic.
- **`internal/logparse`** — the docker log-line parser (`ParseLogLine`, `ScanEndNotice`) shared by the WebSocket log viewer and `api.AppLogs`.
- **`internal/testutil`** — `NewTestDB(t)` spins an ephemeral `postgres:16-alpine` testcontainer, migrates, returns a pool with cleanup.

## Data model

Tables (BIGSERIAL PKs unless noted): `users`, `sessions` (PK token), `organizations`, `members`, `projects`, `environments`, `applications`, `deployments`, `db_instances`, `logical_databases`, `domains`, `backups`, `destinations`, `registries`, `notification_channels`, `metric_samples`, `app_volumes`, `volume_backups`, `app_db_links`, `git_credentials`, `cluster_nodes` (global, not org-scoped), `app_ports`, `node_capacity` (per-node CPU/mem capacity for cluster monitoring), `api_tokens` (org-scoped agent API bearer tokens), `panel_gateway` (singleton, global: the panel domain + gateway secret), `observability_settings` (singleton, global: the node observability agent's push addresses and credentials), `state_backup_settings` (singleton, global: state-backup storage, age recipient, schedule), `state_backup_runs` (30-day journal of state backups).

The migrations themselves are the files in `internal/database/migrations` (`ls internal/database/migrations`); each feature section in these notes names the migrations it added.

Tenancy chain (FK-enforced, ON DELETE CASCADE): `organizations` → `projects` → `environments` → `applications`. `db_instances` hangs directly off `organizations` (org-level, not project/env-scoped); `logical_databases` has two FKs — `instance_id` → `db_instances` (RESTRICT, an instance can't be dropped while it still holds databases) and `environment_id` → `environments` (CASCADE, so deleting an environment deletes its logical databases without touching the instance). `members` joins users↔orgs with `UNIQUE(org_id,user_id)` and role CHECK (`owner`/`admin`/`member`).

- For a `db_instances` row, `app_name` is UNIQUE and is simultaneously the Swarm **service name = internal DNS hostname = volume name prefix** (`<appName>-data`); it's generated once (`dbservice.GenerateAppName`, `krill-<engine>-<slug>-<rand6>`) and never changes, even across a name edit. A `logical_databases` row has no service of its own — it's a database + owner user inside its instance's container (`db_name`/`username`, `UNIQUE(instance_id, db_name)` and `UNIQUE(environment_id, name)`).
- `db_instances.external_port` is a nullable `*int32` (sqlc `emit_pointers_for_null_types: true`); conflicts checked via `CountDBInstancesByExternalPort` / `CountAppPortsByHostPort` (raw TCP/UDP app ports share the same host-port space) and enforced by a partial UNIQUE index (`WHERE external_port IS NOT NULL`, migration 000010 originally, recreated on `db_instances` by migration 000029).
- DB **credentials/passwords are stored plaintext** (no encryption at rest).
- `applications.env_text` (TEXT) is the SINGLE source for env vars — raw, ORDER-PRESERVING `KEY=VALUE` lines (the editor shows `c.App.EnvText`; no re-sorting). The deploy map is derived at runtime (`deploy.parseEnvText`); the old JSONB `env` map column was dropped (migration 000014). `parseEnv` (server) returns the map **and** duplicate keys → `saveEnv` rejects dupes. `applications.domain` has a UNIQUE index; `source_type` CHECK (`image`/`dockerfile`). Advanced container columns (migration 000008): nullable `memory_limit`/`cpu_limit` (TEXT, human units), `replicas` (default 1), `restart_condition` CHECK (`any`/`on-failure`/`none`), `restart_max_attempts`, and `healthcheck_*` (cmd/interval/timeout/retries/start_period). Nullable `command` (migration 000012, TEXT) is an optional container CMD override — `docker.SplitCommand` whitespace-splits it into `ServiceSpec.Args` (overrides the image CMD, keeps ENTRYPOINT; e.g. `start-dev` for Keycloak). Edited on the Advanced tab via `saveAdvanced`.
- `domains` (migration 000009) has `exposed` (BOOLEAN, default false — new domains are internal-only) and `paths` (TEXT, comma-separated path prefixes; empty = whole host) driving Traefik route exposure. `redirect_to_primary` (migration 000060) turns a non-primary domain into a permanent redirect to the app's current primary domain; exposure, paths and protection of that row are then ignored.
- `deployments` is append-only history: `status` CHECK (`running`/`done`/`error`), `trigger` CHECK (`manual`/`webhook`/`schedule`/`api`/`system` — `api` = an agent-API token, `system` = the org-network migration's redeploys; constants in `internal/deploy/trigger.go`, migration 000049), index `(application_id, started_at DESC)`; `ClearOldDeploymentLogs` empties the `log` field for rows finished >1h ago (keeps metadata); `ListDeploymentsByApplication` returns last 50; `applied_config` (JSONB, migration 000065, nullable) is the per-section fingerprint of the configuration a `done` deployment applied (see [deploy.md](deploy.md#saved-settings-that-are-not-live-yet-and-apply-settings)).
- All `timestamptz` map to Go `time.Time`. sqlc config (`sqlc.yaml`): schema `internal/database/migrations`, queries `internal/database/queries`, package `db`, out `internal/database/gen`, `pgx/v5`, `emit_interface`/`emit_json_tags` on.

### Schema notes not covered elsewhere

The per-migration list that used to live in the agent instructions was dropped (read the files
instead); these are the facts from it that the SQL alone does not explain.

- `organizations.owner_id` references `users` with ON DELETE RESTRICT (migration 000011).
- `applications.command` (000012) is the optional container CMD override; `env_text` (000013)
  replaced the JSONB `env` map, which 000014 dropped.
- `notification_channels` is `UNIQUE(org_id, type)` (000015); `metric_samples` holds
  `component`/`cpu_pct`/`mem_bytes`/`mem_limit_bytes`/`ts` (000016), indexed on `ts`,
  `(component, ts)` and, since 000027, `(node, component, ts)`.
- `app_volumes.owner` (000031) is an optional `uid[:gid]` for the chown-on-first-mount fix.
- `app_db_links.field` (000032) defaults to `'url'`; 000033 added `hostport` (`host:port`), 000037
  the S3 fields, all in `app_db_links_field_chk`.
- `cluster_nodes.firewall_managed` (000034) records the worker nftables lockdown.
- `db_instances_engine_check` (000036, widened again by 000056) and `db_instances_status_check`
  (000038, adds the transient `migrating`) follow the drop-and-recreate-wider pattern.
- 000039 adds the partial index `deployments (finished_at) WHERE finished_at IS NOT NULL AND log <> ''`
  so `ClearOldDeploymentLogs` stops scanning the whole append-only table.
- `api_tokens` (000040): `user_id`/`org_id` FKs, SHA-256 `token_hash` + 8-char `prefix` (index
  deliberately NOT unique), `level` CHECK, nullable `expires_at`/`last_used_at`.
- `organizations.network_name` (000042) is written before anything moves;
  `organizations.network_migrated_at` (000043) only once every service has moved, so a pass that
  died halfway is retried.
- 000044 is data-only (it rewrote a stored Docker Hub `minio/minio` reference to
  `quay.io/minio/minio`, keeping the tag or digest); its down migration is deliberately a no-op,
  since rolling back would point instances at a repository that no longer exists.
- `panel_gateway` (000045) CHECKs that a non-off `state` has a host and that direct access can only
  be closed while active; `hsts_max_age` (000046) is CHECK `>= 0` and non-zero only while
  `state='active'`. Its `secret` is written by `panel.EnsureSecret`, not by the migration.
- `observability_settings` (000047): an empty `*_url` (`metrics_url`, `logs_url`) turns that
  pipeline off; `metrics_user`/`metrics_password` and `logs_user`/`logs_password` hold the
  credentials (passwords `secret.Enc`); a CHECK requires an enabled agent to have at least one URL.
- `deployments_trigger_check` (000049) gained `api` and `system`; the previous release renders the
  trigger as plain text, so it tolerates both.
- 000050 replaced the unique `domains_host_key` with a non-unique host index; shared-host rules are
  enforced by `internal/hostshare`, while `applications_domain_key` stays unique.
- `domains_redirect_not_primary` (000060) is the CHECK `NOT (is_primary AND redirect_to_primary)`.

## Migrations

### Migrations must keep the previous release bootable (expand/contract)

A migration may only be additive relative to the release before it: new tables, new columns that are nullable or carry a `DEFAULT`, new non-unique indexes. New unique indexes and foreign keys on existing tables are not additive, an index the previous release relies on for `ON CONFLICT` must never be dropped, and the previous binary must tolerate any new enum/CHECK value the new one writes — widen reads a release before widening writes. `DROP COLUMN`/`DROP TABLE`/`RENAME`/`ALTER COLUMN ... TYPE`/`ALTER COLUMN ... SET NOT NULL`/`DROP NOT NULL`/`ADD COLUMN ... NOT NULL` without a default are only safe one release after the code has stopped using the old shape. Dropping a constraint is always fine; adding a `CHECK` is only safe when it cannot reject anything the previous release writes (the repo's drop-and-recreate-wider pattern, e.g. 000033/000036/000037/000038). The reason for all of this: self-update (see [Self-update](operations.md#self-update)) rolls back to the previous binary on a failed upgrade, and that binary must still start against the schema the newer one left behind — `database.RunMigrations` (`internal/database/migrate.go`) already tolerates this by skipping `Up()` (with a `slog.Warn`) when the recorded schema version is ahead of what the binary embeds, instead of failing with "no migration found".

**Enforcement:** [squawk](https://squawkhq.com) (a Postgres migration linter with a real SQL parser, pinned and checksum-verified by `./tools/get-squawk.sh` into the git-ignored `tools/squawk`) runs as `make lint-migrations` in CI on every `*.up.sql` numbered above `MIGRATION_COMPAT_FROM` (46). `.squawk.toml` keeps only the backward-compatibility rules (`ban-drop-column`, `ban-drop-table`, `renaming-column`, `renaming-table`, `changing-column-type`, `adding-not-nullable-field`, `adding-required-field`, `disallowed-unique-constraint`, `adding-foreign-key-constraint`, `ban-drop-not-null`) and excludes every other rule by name; `assume_in_transaction = true` matches how the file really runs and keeps a UNIQUE/FK added to a table created earlier in the same file from being flagged. The target also self-checks against `internal/database/testdata/squawk/`: `breaking.sql` must report every `-- expect: <rule>` it lists, `compatible.sql` must pass — upgrading squawk means re-checking both and classifying any new rule in `.squawk.toml`. squawk checks the mechanical cases only; reviewers still read every migration, because it does NOT catch a new or tightened `CHECK`, enum widening, dropping an index used by `ON CONFLICT`, or `CREATE UNIQUE INDEX` on an existing table (`disallowed-unique-constraint` only fires on `ADD CONSTRAINT ... UNIQUE`). The escape hatch for an intentional break is `-- squawk-ignore <rule>` on the line directly before the line squawk reports, with the reason and the release from which it is safe after a second `--` on the same line (`-- squawk-ignore ban-drop-column -- safe from v0.5.0: nothing since v0.4.0 reads it`) or on a comment line above it — a comment line *between* the ignore and the statement makes squawk report the statement anyway, and in a multi-line statement squawk reports the offending clause's line (e.g. `DROP COLUMN`), not the statement's first line.

### golang-migrate's `Up()` fails on a schema newer than the embedded migrations

**golang-migrate's `Up()` fails on a schema newer than the embedded migrations** (`versionExists` → "no migration found for version N"). `database.RunMigrations` checks `m.Version()` first and, when the recorded version is ahead of `maxEmbeddedVersion()`, logs a warning and returns without calling `Up()` — this is what makes automatic rollback after a self-update possible (see [Self-update](operations.md#self-update)).

### A failed or interrupted migration defeats the self-update rollback

Golang-migrate marks the schema dirty *before* running each migration and clean after it; a migration that fails, or a process killed mid-migration, leaves it dirty, and `RunMigrations` rejects a dirty schema before its newer-schema tolerance ever applies — so the reverted binary refuses to start too (`database schema is dirty at version N`). Recovery is manual (back up, fix the data/schema, `UPDATE schema_migrations SET dirty = false` once it matches N, or set `version = N-1` to re-run it — see `docs/install.md` Rollback). To keep a *signal* from causing it, `cmd/krill/main.go` runs migrations through `database.RunMigrationsContext` under its own `signal.NotifyContext(SIGINT, SIGTERM)` (the main signal handler is only registered at the end of `run()`): cancelling the context sends `true` on `m.GracefulStop`, which lets the in-flight migration finish and stops before the next one, and the call then returns `migrations stopped by shutdown: context canceled` even if nothing was left to apply. Only SIGKILL (including systemd's after `TimeoutStopSec`) or a crash can still leave the schema dirty.

### `make generate`/sqlc does not prune orphaned generated files

Deleting a `.sql` query file under `internal/database/queries/` and rerunning `make generate` leaves its previously generated `internal/database/gen/<name>.sql.go` in place untouched — sqlc only (re)generates from `.sql` files that still exist, it never diffs-and-deletes. The stale file still compiles (it's just Go + SQL strings, nothing checks the SQL against a live schema at build time), so the drift check stays green and dead code referencing dropped tables silently lingers. `git rm` the orphaned gen file by hand whenever a query file is deleted (e.g. `internal/database/gen/postgres_dbs.sql.go` when `internal/database/queries/postgres_dbs.sql` was dropped in migration 000030's cleanup).

### Unique-violation detection uses `errors.As(*pgconn.PgError)` + code `23505`

**Unique-violation detection uses `errors.As(*pgconn.PgError)` + code `23505`** (`mapUniqueErr` in `internal/org/service.go`), not string matching.

### `external_port` is validated and conflict-checked

**`external_port` is validated and conflict-checked** — `strconv.Atoi` + range `1..65535` → 400 on bad input; pre-check existing ports → 400 on duplicate.
