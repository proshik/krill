# Penpot platform follow-up

Status: intake complete; proposed implementation awaits owner decisions and UI approval.
Owner acceptance: pending. This document is the single backlog/spec and phased plan.
No product implementation, production migration or publication is authorized by it.

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

## Open decisions requiring owner approval

1. **UI/entity:** System entity (recommended) versus labels; name, membership,
   ordering and the concrete expandable-card interaction above. No schema/UI build
   until a mockup with expanded, collapsed and ungrouped states is accepted.
2. **Managed service contract:** Silo variant under existing engine (recommended
   first for compatibility) versus explicit engine with staged compatibility;
   env bucket ownership versus org resource with env grants; cache-only/durable
   Valkey and migration downtime. Silo itself is already chosen.
3. **Recovery contract:** bucket snapshot RPO/RTO, retention, overwrite/versioning
   behavior and allowed cutover downtime. Recommend isolated restore before any
   destructive restore, explicit retention opt-in for adopted backups.

## Phased implementation and safe Penpot adoption

- [ ] Parent accepts intake scope/evidence; owner resolves the three decisions.
- [ ] Produce and approve concrete UI mockup in existing Krill style. Refine this
  document, not a competing plan; no dependency installation at intake.
- [ ] P0 disposable-cluster reproduction, network migration design and firewall
  compatibility checks. Ship independently of native-resource adoption.
- [ ] P1 additive service/subresource contracts and explicit Valkey support;
  previous-release read/rollback compatibility checks before new enum writes.
- [ ] P1 grouping and environment graph projection, UI/API/RBAC tests; existing
  environments/apps remain ungrouped until explicitly organized.
- [ ] Bucket backup/restore and diagnostic work; prove isolated recovery first.
- [ ] Obtain separate production migration approval with measured downtime and
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

## Intake completion gate

Deliver this document and a local commit to the parent for acceptance. Source/link
and privacy checks are required; repository policy also requires `make test` before
commit. Live scenarios above remain UNVERIFIED until their implementation phases.
The intake ends with handoff, not background implementation.

Local verification: relative code/doc links, whitespace and private-address/path/token
scan passed. During the required `make test`, `TestS3Roundtrip` and
`TestS3CheckAccessBlocksLoopbackByDefault` failed to create MinIO because the
registry manifest HEAD returned 401 for the pinned default image. This independently
reproduces image unavailability for this test environment; it does not establish
permanent registry status. Product sources were unchanged by this intake.

Final `make test` result: FAIL. The backup, server and volume packages had
MinIO-dependent failures on the same registry 401; the full suite was allowed
to finish. Documentation checks passed; no product code was changed to bypass
these failures.
