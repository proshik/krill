# Penpot platform follow-up

Status: implementation in progress (owner handed review and implementation to the
Claude Code session on 2026-11-06). This document is the single backlog/spec and
phased plan. It authorizes local implementation, local commits and disposable
verification only; production migration, push, merge, release and deploy each
need a separate explicit owner decision.

## Owner request and scope

Group deployed systems logically within an environment: Penpot, lets-project and
base13 must no longer appear as one flat collection. Show the resources each
system actually uses, down to component links. Run Penpot on native managed
Postgres, S3 and Valkey, without manual bucket/user management or rclone jobs.
Silo is the owner's selected S3 implementation; replacing it is not in scope.
The requested outcome does not require moving apps between projects/environments.

Priority: P0 network/firewall safety; P1 managed S3/Valkey and environment grouping;
then backup, diagnostic and recovery improvements. These are work packages of this
one task, not separate issues. All designs below are proposals unless identified
as owner requirements. Shared filesystem support and scheduler redesign are
investigations, not prerequisites or promised features.

## Evidence and limits

Read-only intake on 2026-11-06 used deployment records at commit `418ea1b`:
machine runbooks, network/MSS and secure-join scripts, Penpot component manifests,
frontend recovery runbook, S3/Postgres instance records and observability notes.
Those private records are not distributed here. No host access, credentials,
private paths or screenshot are included. Observations below are historical
operator evidence, not a new live audit or a current registry availability claim.
Neutral machine names are cp, worker-db and worker-app.

- Deployed Krill was v0.5.0. Six Penpot components and Silo run on worker-app
  (4 CPU, 8 GiB RAM, 150 GB disk); existing PG17 runs on worker-db. Five Penpot
  images are 2.18.6; Valkey 8.1.4 is the sixth component, currently an ordinary app.
  The logical Penpot database and role already use native PG db-links.
- Overlay MTU 1450 over WG MTU 1420 produced frag-needed/PMTU 1370 for a
  DF1450 probe from Traefik; DF1350 passed. Large requests took 8–14 seconds or
  timed out. Narrow bidirectional MSS1330 rules on the organization's vxlan0
  bridge reduced them to 0.6–1.4 seconds. A 30-second timer repairs namespace
  recreation. This demonstrates a network configuration/diagnostic gap, not a
  universal magic MTU or permission to recreate existing overlays.
- cp had no documented `inet krill` table, UFW was inactive and INPUT accepted.
  Why the table disappeared is unknown: neither reboot nor self-update is proven
  responsible. The stock drop-policy ruleset lacks WG UDP51820 allowance.
  A narrow rollback-protected ruleset preserved SSH/HTTPS and UI through
  docker_gwbridge, restricting cluster traffic to wg0.
- The default MinIO image returned 401 during deployment. The chosen
  `pgsty/silo:RELEASE.2026-10-26T00-00-00Z` now runs as a managed `engine=minio`
  instance with image override, placement, volume and stop-first. It is not an
  ordinary app. Do not return to archived MinIO or silently select another product.
- Bucket `penpot` and restricted user `penpot-assets` were created manually;
  backend uses restricted credentials and another-bucket access returned 403.
  Existing S3 db-links supply instance root credentials. Assets are backed up by
  rclone/systemd to an existing B2 destination under a separate prefix, without
  automatic retention. Snapshot check and two downloaded files had zero differences.
  Native PG daily backups with retention 14 have several successful runs.
- Backend needed 3 GiB/2 CPU; exporter Chromium OOMed at 1 GiB and now has
  2 GiB/1 CPU. The 512 MiB default is not inherently a platform bug. `/readyz`
  and a 90-second start period were configured manually. Replicas running alone
  do not prove readiness.
- Some deploy requests were refused; automation initially treated them as queued.
  The precise refusal cause remains unproven without logs. Image pulls on cp
  serialized stack progress through the single deploy worker.
- First reboot left an old backend PG connection holding an advisory migration
  lock. Only old Penpot sessions were terminated. Role keepalives became 30/10/3;
  pool min/max became 5/15 instead of default max60 against PG max100 during
  overlapping releases. This is app/PG behavior, not a proven generic Krill bug.
  Second reboot recovered automatically; browser reopen and PNG export passed.
- Per-app volume naming prevented a frontend/backend shared volume; S3 solved
  this scenario. Shared filesystem work must remain a separate proposal.

## Current code status (local master, not deployed version)

Compared local master/working baseline `70c329c` with v0.5.0 `e1a03e8`.
No remote freshness claim is made. No OpenSpec or project skill directory was
present; this public-safe document is the canonical artifact.

| Area | Existing capability and remaining boundary |
| --- | --- |
| Topology | [graph builder](../../internal/topology/graph.go), [adapter](../../internal/server/topology_handlers.go) already implement org-scoped app↔DB links, ingress, placement and DSN detection. Reuse these primitives; environment overview grouping is the gap. |
| Environment UI | [project template](../../internal/web/templates/project.templ) presents environment applications/resources. Add grouping here in the established design system, not a replacement topology application. |
| Drivers | [drivers](../../internal/dbservice/drivers) contain Postgres, Redis, DragonFly, MinIO; no managed Valkey. Redis args hardcode `redis-server`, so swapping its image is insufficient. MinIO link fields resolve instance superuser/password, not bucket credentials. |
| Lifecycle | [instance service](../../internal/dbservice/instance.go) already provides placement, volume and stop-first lifecycle. Silo needs supported-image validation and subresources, not a second generic instance manager. |
| Firewall/network | [ruleset](../../internal/firewall/ruleset.go) allows service TCP ports but no configurable WG listener UDP allowance; [Docker client](../../internal/docker/client.go) and [join](../../internal/cluster/join.go) are integration points for MTU work. |
| Queue | [handlers](../../internal/server/app_handlers.go) reject Enqueue result zero, including v0.5.0; [deployer](../../internal/deploy/deployer.go) uses one worker. Do not describe this as proven silently lost deploys. REST/MCP already expose accepted deployment IDs/status. |
| Newer master | [deployment guide](../guides/deployments.md) documents pre-deploy commands and release rollback; [environment guide](../guides/environments.md) documents cloning. These landed after v0.5.0 and must be reused. Rollback does not restore database/volume data and checks link identity/credentials. |
| Backups/volumes | [backup](../../internal/backup) and [volume backup](../../internal/volume) already exist; bucket-object backup lifecycle is missing. [VolumeName](../../internal/docker/volume.go) includes app ID. |

## Implementation review (2026-11-06)

Reviewed against the sources at `6a677ec`. Corrections and additions to the
intake, each checked in code:

- **Baseline.** `70c329c` is tag `v0.6.0`; pre-deploy commands, release rollback
  and environment cloning are released, not "newer master". The previous release
  for the next one is v0.6.0, but production still runs v0.5.0, so a direct
  v0.5.0 → next upgrade would roll back to v0.5.0. Both must be considered.
- **Unknown engine crashes the previous release.** v0.5.0 and v0.6.0 call
  `drivers.Registry.MustGet(engine)` in `instanceSpec`, `reconcileProxy` and
  instance removal; an unregistered engine returns nil and the call panics. The
  deploy runs in a goroutine without a recover, so a Deploy/Start/version change
  on a `valkey` row after a rollback kills the whole process (listing and detail
  pages use `Registry.Get` and survive). Owner decision: ship Valkey in one
  release, document that a rolled-back binary must not act on Valkey instances,
  and make the new code tolerant (unknown engine → error, never panic) so later
  engines are safe to roll back over.
- **Silo stays `engine=minio`.** No new engine value; subresources (buckets,
  principals, bucket links) live in new tables the previous release ignores.
  Bucket links do not reuse `app_db_links` (widening its XOR CHECK would make the
  previous release's link resolver see rows it cannot resolve); a separate table
  is invisible to it. Under rollback, a redeploy loses the injected bucket
  variables; documented.
- **Firewall is not persisted.** `firewall.Apply` loads the ruleset with
  `nft -f` only; nothing writes it to disk or reloads it at boot, so a reboot
  always leaves a node without `inet krill`. This is a confirmed code gap, not
  proof of why the historical table disappeared. Worker status on the Firewall
  page comes from the stored `firewall_managed` flag, never from the host, so a
  missing table on a worker is invisible.
- **Firewall and WireGuard.** The drop-policy ruleset admits no UDP listener for
  WireGuard, and swarm ports (2377/7946/4789) are admitted by source address on any
  interface, although VXLAN is unauthenticated. Admitting WireGuard listen ports
  (authenticated by WireGuard itself) and binding swarm ports to the WireGuard
  interface when the swarm runs over it closes both.
- **Overlay MTU.** `NetworkEnsure` creates every overlay with Docker's default
  (1450, assumes a 1500 underlay). The existing network's MTU cannot be changed in
  place, and its name is derived from the organization id in v0.6.0 (gateway
  attachment in `cmd/krill/main.go`, `orgnet.Migrator`), so moving an organization
  to a differently named network breaks ingress after a rollback. Recreation must
  keep the name: a two-hop move through a temporary network the gateway also joins.
- **Deploy refusals.** `Deployer.enqueue` returns 0 for every refusal (app busy,
  org cap, queue full, store error, no worker); callers cannot tell them apart.
  Not a lost-deploy bug; typed reasons are the gap.
- **S3 admin client.** madmin-go is AGPL-3.0 (Krill is MIT). Owner decision: a
  small own client for the MinIO admin API (user, canned policy, policy attach)
  on the existing aws-sdk-go-v2 signer and golang.org/x/crypto; buckets through
  aws-sdk-go-v2. Compatibility is proven against the Silo image in integration
  tests.

## Owner decisions (2026-11-06)

1. **Systems UI: option A.** Env-scoped "System" entity; an app belongs to at
   most one system; collapsible cards with a resource row and component cards;
   selecting a component highlights its links, selecting a resource highlights
   its consumers; an "Ungrouped" section; the environment's database list stays.
   Removing a system never touches apps, resources or data; a shared instance is
   badged "shared" and owned by nobody. Detailed states (empty, error, narrow
   viewport, keyboard) are reviewed on the running implementation.
2. **Valkey: one release, durable by default** (volume `/data`, AOF + RDB,
   stop-first + flock, no backups in v1, same as Redis). Rollback limitation
   documented, see the review above.
3. **S3 admin: `mc` through Engine.Exec** (revised once the Silo image was found
   to ship `mc`; the first choice, an own admin-API client, would have needed
   madmin's body encryption). Silo remains the chosen product.
4. Still open, decided at their stage: bucket snapshot RPO/retention/overwrite
   contract; automated existing-network MTU migration versus a documented
   procedure; ownership scope of buckets (proposal: env-scoped like logical
   databases, inside an org-scoped instance).

## Proposed work packages and acceptance scenarios

### P0: network and firewall

- Inventory effective underlay/WG/overlay MTU and provide bounded cross-node PMTU
  probes and actionable failures before join/deploy. Configure new networks from
  a validated budget. Existing-network changes require an explicit rolling
  migration, affected-service inventory, downtime estimate and rollback plan.
- Add WG-aware ingress policy with configurable interface/listener, preserving
  unrelated host services and existing firewall ownership. Report desired versus
  observed rules, missing table, persistence and last successful verification.
  Reuse the current dead-man switch; never overwrite an unconfirmed snapshot.
- Acceptance: disposable cp/worker-db/worker-app with constrained WG reproduces
  large-response failure before remediation and transfers large responses after;
  reboot and Docker namespace recreation retain the solution without manual
  nsenter/timer repair. SSH/HTTPS/panel and unrelated listeners survive; WG joins
  work, cluster ports are inaccessible outside the approved interface. Missing
  table/drift is visible. A failed confirmation restores prior access. No claim
  that these checks establish the historical cause of table disappearance.

### P1: native S3 and Valkey

- Support the chosen Silo image as a documented managed S3 variant or driver;
  verify pull on supported architectures, digest pinning, health, API compatibility,
  upgrade and rollback on disposable data. Record a tested image matrix at implementation
  time; historical 401 does not establish permanent registry status.
- Model buckets, restricted principals/policies and encrypted credentials, with
  create/import/rotate/revoke/delete lifecycle. Links target bucket-scoped credentials
  by default; root remains explicit privileged administration. Import existing
  bucket/user without recreation. Prevent deletion while linked, or require explicit
  unlink/data-destruction confirmation. API/UI obey tenant and role boundaries.
- Add explicit managed Valkey support with the correct executable/entrypoint,
  auth, healthcheck, resources, placement, persistent-volume semantics and links.
  Decide cache-only versus durable mode and backup expectations before shipping.
- Acceptance: create or adopt Silo bucket/user, link backend through UI/API, and
  read/write only that bucket; another bucket returns 403. Rotation is staged and
  consumers verified before revocation. No secret in member views, graph, logs or
  agent reads. Valkey starts, authenticates, survives restart per chosen durability
  contract and migrates from ordinary app without changing endpoint before ready.
  Previous Krill release can boot/read schema and existing engine records after
  upgrade; new enum/CHECK values require staged read compatibility before writes.

### P1: environment systems and resource relationships

Owner requirement: systems aggregate real component dependencies; physical DB
instances remain org-scoped, logical DBs env-scoped, nodes site/instance-scoped.
A shared instance never becomes exclusively owned by a group. Group removal
must not delete apps, shared resources or data by implication.

Recommended option A: an env-scoped **System**, optional membership per app,
with collapsible cards. Option B: named app labels with saved grouping in the
environment view; less schema but ambiguous lifecycle/multi-membership. Recommend
A for stable resource aggregation; approve naming and one-system-per-app first.

Concrete interaction target for owner review (not an approved UI):

```text
side / prod                       [Create system] [Ungrouped (N)]
v Penpot       readiness summary, not only running count
  PG: penpot @ shared-postgres   S3: penpot @ Silo   Cache: Valkey
  frontend   backend   exporter   admin-console   MCP
  Select backend -> its PG, bucket and cache links; no credential values
> lets-project   PG: lets_project @ shared-postgres
> base13         No linked resources
```

Managed Valkey appears as a resource after adoption, not a duplicate app component.
Expanded cards retain existing app actions; resource chips open authorized detail.
Clicking a resource highlights consuming components; collapsed cards aggregate
and deduplicate their links. Shared-instance badges explain cross-system use.
Reuse topology graph/link types and existing UI tokens/components, with an
environment-filtered projection and server-side authorization before serialization.
Do not expose other environments through a shared instance's logical DB list.
Keep explicit modeled links authoritative; existing DSN detection, if displayed,
must be marked detected and never infer ownership or links from similar names.

Acceptance: Penpot, lets-project and base13 are separately visible in the same
environment; the shared PG instance is referenced twice without duplication of
ownership. Component selection reveals exact links. Ungrouped apps stay reachable;
moving apps in/out changes no deployments. Empty/no-link/error states, keyboard
expand/select, narrow viewport and EN/RU labels are reviewed. Members see only
permitted metadata; admins manage grouping; cross-org/env IDs return 404. Group
removal preserves apps/buckets/PG data. Existing topology remains operational.
Approve a concrete mockup and interaction states before product implementation.

### Backup, diagnostics and recovery

- Native bucket snapshots/restore with destination/prefix isolation, schedules,
  explicit retention and failure notifications. Specify object metadata/versioning,
  consistency during writes and restore overwrite semantics; a mirror is not a
  point-in-time backup. Reuse existing destination/scheduler/notification plumbing.
  Acceptance: restore to an isolated bucket, compare manifest/checksums and export
  a document; retention touches only this backup's objects and concurrent runs
  are guarded. Existing PG and other B2 backups remain intact.
- Surface effective inherited/explicit limits, OOM cause, health/startup progress
  and readiness. Test slow /readyz and Chromium-like OOM separately from running
  replicas. A heavy-stack recipe is optional, not a global default increase.
- Introduce typed refusal reasons where missing (app busy, org cap, queue full,
  shutting down) consistently across UI/REST/MCP. Acceptance: rejected requests
  never appear pending; accepted ones have a pollable ID; no image/tag mutation
  on rejection. Scheduling/prefetch on selected nodes needs measurement and design;
  do not promise bounded parallelism before lock/resource analysis.
- Offer connection-budget/lock diagnostics and consider app deployment strategy
  selection (engine stop-first already exists). Test overlap against PG capacity.
  Never automatically kill arbitrary PG sessions. Use existing pre-deploy and
  release rollback where appropriate, without claiming they undo schema/data.

## Phased implementation and safe Penpot adoption

Stages, each a local commit series with its own checks:

- [x] Review of the intake against the sources; owner decisions recorded above.
- [x] **S1 firewall** (`59e8978`) — persisted ruleset (written only after confirmation,
  reloaded at boot by a `krill-firewall` unit, removed by Open), WireGuard listen
  ports admitted, swarm ports bound to the WireGuard interface when the swarm
  runs over one, live per-node status with drift (missing table, ruleset differs
  from the expected one, not persisted, revert pending). Dead-man switch reused.
- [x] **S2 overlay MTU** (`59e8978`; automated recreation of existing networks
  not built, a documented same-name procedure instead — owner decision pending) — `KRILL_OVERLAY_MTU` for networks Krill creates
  (unset keeps Docker's default, so existing installs are unchanged); per-node
  underlay inventory and DF probes on the Network page with a recommended value;
  warning when a joining node's budget is below the configured MTU. Existing
  networks: design of the same-name two-hop recreation with inventory, downtime
  estimate and rollback; implemented only after the owner chooses it over a
  documented procedure.
- [x] **S3 engine tolerance + Valkey driver** (`a5711d6`) — unknown engine is an error, not
  a panic; `valkey` engine (CHECK widened), correct `valkey-server` entrypoint,
  auth, healthcheck, durable volume, links identical to redis.
- [x] **S4 Silo/S3** (`2cb2085` Silo default + test image; buckets commit below) — supported image as the MinIO default (test image too),
  buckets, restricted principals/policies, bucket-scoped links by default, import
  of an existing bucket/user, rotation, delete guards.
- [x] **S5 Systems UI** (option A; checked in the running app at desktop and
  768px, keyboard focus highlights, EN/RU, RBAC and 404 tests) — option A, reusing topology link types; RBAC, 404 on
  cross-org/env ids, EN/RU, keyboard, narrow viewport; checked in the running app.
- [ ] **S6 diagnostics** — typed deploy refusals across UI/REST/MCP (done; a full
  queue no longer marks the app errored); effective
  limits, OOM and readiness on the app page; connection-budget hints (no session
  killing).
- [ ] **S7 bucket backup/restore** — after the recovery contract is decided.
- [ ] Separate production migration approval with measured downtime and
  rollback deadline. This plan grants none.

Before cutover, inventory and securely preserve PG database/role, Penpot master
key, accounts/documents, bucket objects/policy/user, restricted credentials,
image digests/pins, DNS, links and backup snapshots. Do not print these values.
Verify PG and bucket restore to isolated targets. Adopt Silo in place wherever
possible; do not recreate its volume or bucket. Import restricted links and stage
credential rotation separately. Provision managed Valkey on intended placement,
choose data transfer or explicitly approved cache discard, and verify health/auth.
Keep existing endpoints active until replacement readiness and client checks pass.

Use a maintenance/write-freeze window if required for consistent PG/assets capture;
record the measured maximum outage before approval. Switch links/endpoint only
then; verify login, existing documents/assets, new writes, browser reopen and PNG
export, followed by a controlled reboot recovery on a disposable rehearsal first.
Validate backup jobs after cutover. Retain old configuration and snapshots through
an agreed observation window. Rollback restores old endpoints/links and service
configuration; if writes occurred, reconcile or restore a paired PG/assets snapshot
under explicit data-loss approval. Release rollback alone is insufficient and may
refuse after db-link changes. Do not delete old cache/backup jobs until replacement
recovery is proven; stop overlapping writers/jobs during the transition.

## Verification log

Local and disposable only; nothing touched production.

- Firewall: generated rulesets, the persisted copy and the probe were run on real
  nft and kernel WireGuard in a throwaway privileged container (load, idempotent
  reload, reload of the persisted copy, marker read back, wg0 MTU 1420 and port
  51820 detected, Open removing the copy). A real reboot and systemd ordering are
  UNVERIFIED until a disposable VM run.
- Overlay MTU: `TestOverlayNetworkMTU` on the local swarm reads MTU 1370 inside a
  container on a network created with `KRILL_OVERLAY_MTU=1370` (the Docker option
  is the underlay MTU, found by this check). The same-name recreation procedure
  and the before/after large-response reproduction over a constrained WireGuard
  cluster are UNVERIFIED.
- Valkey: `TestValkeyDriverOnRealImage` (Debian and alpine tags): valkey user,
  authenticated healthcheck, AOF data across containers, directory lock.
- Silo buckets: `TestBucketLifecycleOnSilo` on the local swarm: restricted key
  denied on another bucket, import without recreation, rotation and revoke,
  removal. UI checked in the running app (bucket section, bucket page, linked
  buckets, root-credential marking, Firewall page with the MTU section).
- `make test`-equivalent: the earlier MinIO 401 baseline is gone with the Silo
  test image.

## Intake verification (historical)

The intake's `make test` failed in the backup, server and volume packages because
the pinned MinIO test image returned 401 from its registry. Fixing the supported
S3 and test image is part of S4; until then those failures are a known baseline,
not regressions of this work.
