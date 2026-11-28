---
id: KRL-002
title: Krill link egress — apps reach allowed foreign TLS APIs through the relay without code changes
kind: feature
status: backlog
priority: 2
component: krill
depends_on: [KRL-001]
touches:
  - internal/link/**
  - internal/docker/**
  - internal/deploy/**
  - internal/traefik/**
  - internal/selfupdate/**
  - internal/database/migrations/**
  - internal/database/queries/link*.sql
  - internal/database/gen/**
  - internal/server/**
  - internal/web/**
  - cmd/krill/**
  - docs/internals/networking.md
  - docs/guides/link.md
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-27"
after_release: "435dc5371ac3925b11f03f11a6ea591fc8c8f7b5"
---

## Goal

An app on the client install calls `https://api.telegram.org` (long polling
`getUpdates` included) or an LLM API such as `https://api.openai.com` by its usual
name, and the connection goes through the link and leaves from the relay. No change
to the app's code or env. TLS stays end to end: the relay never sees content or bot
tokens. The mechanism is general: any exact host name that speaks TLS.

Owner decisions: destinations are a general allowlist, not Telegram only; the
relay's list is the hard boundary; the client operator grants the link to an
organization, and an org admin of a granted organization enables it per app.

**Release ordering:** ship this in a later release than KRL-001 — `after_release` is
KRL-001's merge commit (`435dc53`), so this task starts only once a release contains
the link core. A rollback by one release from this release then lands on KRL-001's
deployer, which keeps the `krill-egress-*` attachments, and its listener refuses egress
streams: fail closed. An update that skips the core release leaves an older binary as
the rollback target; the gate below keeps egress off until it is gone.

## Acceptance

- [x] WHEN the migration runs THEN it adds `link_destinations (host, port, PK both)`
      (relay only — the single destination list), `link_org_grants (org_id PK →
      organizations ON DELETE CASCADE)` and `app_link_destinations (application_id →
      applications ON DELETE CASCADE, host, port, PK all three)`, additive only. Hosts
      are normalized like the panel domain (no IP, no wildcard), ports with
      `strconv.Atoi` and 1..65535 (400 otherwise).
- [x] WHEN the relay operator edits the relay's destination list THEN the relay sends
      it to clients in `control` (`hello`, `lists`) and refuses any `egress` stream
      whose `{host, port}` is not on it; it reads the stream's ClientHello itself and
      refuses unless the SNI equals the declared host (no SNI → refused), so a
      compromised client cannot reach another virtual host behind an allowed name's
      shared TLS front end; it resolves the host itself and dials only through
      `internal/netguard` with private egress disallowed.
- [x] WHEN the client operator grants an organization and an org admin there selects
      destinations from the relay's list THEN the client keeps no list of its own (the
      app page offers only what the relay sent), the selection is saved in
      `app_link_destinations`, the app shows "settings not applied" (add the selection
      to `configSections`/`Deployer.configFingerprint`), and the card says in one line
      that a selected host is intercepted on every port (unselected ports are
      refused, never sent direct). Without a grant there are no controls and the POST
      is 404.
- [x] WHEN such an app is deployed THEN it gets its own overlay `krill-egress-<appID>`
      (MTU as `orgnet`) as a second network, Traefik keeps routing it over the org
      network through the existing `traefik.docker.network` label — never add a
      `traefik.swarm.*` label beside it, Traefik v3.6.1 skips a service carrying both
      kinds — and an integration test on real Traefik proves an app on two networks
      still answers on its domain.
- [x] WHEN the sidecar is reconciled THEN `krill-link-egress` (image `traefik:` +
      `traefik.TraefikVersion`, `node.role==manager`, **endpoint mode DNSRR**, no docker
      socket, no swarm provider) is attached to every `krill-egress-<appID>`; on each
      network its aliases (new `docker.ServiceSpec.NetworkAliases`,
      `json:",omitempty"`) are the union of that app's **saved and applied** selection
      (applied = what the running deployment was built with, from
      `deployments.applied_config`), so a host leaves DNS only after a deploy without it
      has converged; one entrypoint per distinct port; an HTTP provider at
      `/_krill/link/egress-config` guarded by its own HMAC token derived from the panel
      secret; dynamic config routing `HostSNI(host)` with passthrough to the
      authenticated gateway listener (PROXY v2 + inline sidecar client cert). The spec
      is fingerprinted so unchanged inputs do not recreate the task.
- [x] WHEN the sidecar hands a connection to Krill THEN Krill parses the ClientHello
      SNI itself, identifies the app by the PROXY v2 destination address (the
      sidecar's address on that app's network), requires the source address to be a
      task of that app on that network (task addresses from a new engine method; on a
      miss one refresh, then refuse), and opens an `egress` stream `{host, port,
      service}` only when: the session is up AND the app's organization is granted AND
      `(SNI, port)` is in the app's saved selection AND on the relay's list; otherwise
      it resets the connection and logs a WARN without app data. Revoking a grant or a
      destination closes new connections at once, without a redeploy.
- [x] WHEN the link is down, the sidecar is scaled to 0, or a destination was removed
      THEN the app's connection is refused; it never resolves the real address and
      never goes direct.
- [x] WHEN the role is `client` THEN the sidecar is reconciled at startup; disabling,
      revoking or switching the role to `off` empties the sidecar config, the sidecar
      service is removed only when no task is attached to any `krill-egress-*` network
      (a removed service drops the alias and DNS falls back to the real address — fail
      open), and an app's network is removed only when none of its tasks use it.
- [x] WHEN the client is connected THEN it sends a `manifest` (app id,
      `project/environment/app`, destinations) on connect and on change, and both
      Link pages show a per-service table: destinations with state, bytes up/down
      since process start. The relay shows the client's services under its peer card;
      they never appear in the relay's topology.
- [x] WHEN the client operator grants an organization, or an org admin saves an app's
      destinations, while the rollback binary is older than the link core release THEN
      it is refused and nothing is written. The rollback binary is `<bin>.prev`, the one
      both **Roll back** and the automatic revert put back. A new `selfupdate.Updater`
      method reports it as one of: **absent** (no `krill.prev`, nothing to roll back to),
      **a release** (`krill.prev --version` through `execVersion` + `parseVersion`,
      cached per file like `prevCache`), or **unknown** (binary path unresolved, updater
      not wired, `--version` fails or times out, or prints no `krill vX.Y.Z`). It must
      not reuse `Previous`, which answers false alike for no file, an unreadable one and
      one not older than the running binary. Egress may be enabled only when the binary
      is absent or `semver.Compare(prev, link.CoreRelease) >= 0`; unknown refuses (fail
      closed). `link.CoreRelease` is the first `vX.Y.Z` tag containing `435dc53`, known
      when this task starts. The Link page's grant control and the app's link card are
      disabled with the reason — the `krill.prev` version (or "cannot be read"), the core
      release, a link to Settings → Updates — through new keys in the `en` and `ru`
      catalogs; each POST checks again and answers with a flash of the same reason.
- [x] WHEN the docs are updated THEN `docs/internals/networking.md` gets the
      "Krill link" egress part and `docs/guides/link.md` explains setup with
      Telegram and an LLM API as examples and the limits (TLS only, exact names,
      host-level interception, no MTProto).

## Checks

- Unit: rollback gate table with `execVersion` stubbed — no `krill.prev`, older than
  the core, equal, newer, `--version` error, timeout, output without `krill vX.Y.Z`,
  unresolved binary path; the grant and selection POSTs refused with the flash and no
  row written while the gate is closed; the reason rendered in `en` and `ru`.
- Unit: decision table (session × grant × app selection × relay list); relay checks
  (list, SNI equals host, netguard); app identification by destination and source
  address; aliases = saved ∪ applied; SNI parser (fuzz); sidecar args and dynamic
  config; `configFingerprint` changes with the selection.
- Integration (`-tags=integration`, Colima env from AGENTS.md): Swarm + real
  `traefik:v3.6.1` sidecar + the Krill listener on the test host — a container in
  `krill-egress-<app>` opens TLS to an aliased name and reaches the listener with its
  own task address in PROXY v2 (from a worker node too where the suite has one); a
  container of another app or organization does not resolve the alias; a destination
  removed from the selection is refused, not resolved to the real address, until the
  deploy without it converges; an app on org + egress networks still answers through
  Traefik; the sidecar scaled to 0 refuses; an end-to-end egress stream to a local TLS
  server through two in-process link endpoints, and a stream whose SNI differs from
  its declared host is refused by the relay.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- TLS only, exact host names, no wildcard, no UDP, no MTProto; interception is per
  host, not per port.
- Aliases never go on `krill-org-<id>` or any shared network: one egress network per
  app keeps tenants and apps apart, and apps without egress keep resolving real
  addresses.
- No API (KRL-004), no Krill notifications (KRL-003), no ingress (KRL-005).
- A change of aliases recreates the sidecar task (StartFirst); its gap is measured in
  KRL-007, not engineered away here.
- The rollback gate is checked when egress is enabled; it does not revoke an existing
  grant. Through Krill `krill.prev` can only become the binary that ran here, which is
  at least the core release once egress is on. A binary put in place by hand, or a rollback
  of the container image to an older tag, is outside it (the `docs/install.md` warning).

## Context

- Verified 2026-11-27 on a local Swarm (Docker 29.5.2): a network alias with dots
  resolves from other services of that network and services outside it get the real
  address; through a VIP the server sees the network's load-balancer address, not the
  client task's, while DNSRR keeps the task address; a DNSRR alias of a real name
  (`example.com`) scaled to 0 does not resolve at all; a removed service makes the
  name resolve to the real address again.
- Traefik v3.6.1 `pkg/provider/docker/shared_labels.go`: both `traefik.docker.*` and
  `traefik.swarm.*` are read; both present → "both Docker and Swarm labels are
  defined", the service is skipped.
- Rollback target: `docs/install.md` [Rollback](../docs/install.md#rollback) — the
  paragraph on rolling back below the Krill link core release and on an update that
  skips it. `internal/selfupdate/install.go:178` keeps the running binary as `.prev`
  before the swap; `internal/selfupdate/revert.go:19` (the automatic revert) and
  `Updater.Rollback` (`checkRollback`, `internal/selfupdate/updater.go:359`) put
  `<bin>.prev` back; `Updater.Previous` (`updater.go:410`) filters by `olderThanCurrent`;
  `install.sh` removes `krill.prev`.
- `internal/deploy/deployer.go:1204` (`Network: net`), `internal/docker/spec.go`
  (attachments, no aliases), `internal/traefik/labels.go:146`,
  `internal/netguard/netguard.go`, `internal/orgnet`.

## Implementation notes

Recorded by the implementing session (2026-11-28). Decisions marked *(impl)* are the
implementer's and reversible.

- `link.CoreRelease = "v0.17.0"`: `git tag --contains 435dc53` lists `v0.17.0` first.
- Owner decision (2026-11-28): no `krill.prev` at all → egress may be enabled; a version that
  cannot be read (unknown) → refused.
- *(impl)* The sidecar presents the installation's gateway client certificate
  (`link_settings.gateway_cert`, the one the relay's gateway uses): spec §5.4 has the sidecar
  play the gateway on a client, and a client's main gateway never receives that certificate
  (`GatewayTCP` publishes nothing outside the relay role). The listener hands the whole PROXY
  header (source and destination) to the role: a relay serves link sessions, a client egress.
- *(impl)* The applied selection is stored in plain form beside the digests in
  `deployments.applied_config` (`egress`: `host:port` list, not a secret); its digest is the new
  section `link`. A fingerprint recorded before has no `link` section: unknown, never "changed".
- *(impl)* The selection a deploy applies is the saved one while the role is `client` and the
  organization is granted, otherwise empty: revoking a grant or turning the role off marks the
  apps "settings not applied", and their next deploy leaves the egress network. Aliases are the
  saved selection (whatever the grant) ∪ the applied one, so DNS stays on the sidecar until a
  deploy without the host has converged.
- *(impl)* The deployer now manages the app's own `krill-egress-<appID>` attachment itself: it
  lists the network when the applied selection is not empty and drops it otherwise, so it no
  longer sets `KeepNetworkPrefix` (that rule protects a rollback onto the KRL-001 deployer, which
  still keeps every attachment). Without dropping, an app whose link was turned off would stay
  refused for good.
- *(impl)* Before an app deploys onto its egress network the deployer asks the link to create
  the network and to have the sidecar serve it with the app's aliases, and waits until the
  sidecar's new task runs (as `traefik.Reconcile` does); a failure fails the deploy rather than
  start the app on a network whose names would resolve to the real addresses.
- *(impl)* The pre-deploy command (a one-off container on the organization network) does not
  use the link: Krill could not identify it as a task of the app. Documented as a limit.
- *(impl)* Capabilities on both sides: the client's `hello` and the relay's `hello` carry
  `egress: true`; only then does the client send `manifest` and the relay send `lists`, so a
  KRL-002 installation and a KRL-001 one keep their session (a KRL-001 relay closes the session
  on an unknown control message).

- *(impl)* The sidecar's static configuration (entrypoints, the HTTP provider and its token)
  is a Swarm secret named by its content and passed as `--configFile`: the token must not be in a
  container's argv or labels. Traefik ignores the CLI once a file is given, so all of it is in the
  file; with no port left `entryPoints` is left out (an empty object stops Traefik v3.6.1 — found
  by the Swarm test).
- *(impl)* Granting an organization and saving an app's selection are both refused under a
  closed gate (the task's wording); revoking is never gated. A saved destination the relay no
  longer offers can be kept or dropped; one it never offered is refused.
- *(impl)* A grant does not need a paired relay: the Link page shows the egress sections for any
  client.
- *(impl)* The egress object names (sidecar, network prefix, app service, secret label) are
  fields of `link.Egress` defaulting to the production ones, so the Swarm integration test runs
  with `krl002-` names beside other installations on a shared Swarm.
- *(impl)* Manifests are bounded (256 services, 64 destinations each, one 16 KiB control line):
  they are for display; the relay's list is the boundary. Counters are in memory, keyed by
  peer/service/destination, at most 4096 keys a side.
- Observed on the Swarm test: right after the sidecar is scaled to 0 its stopping task can still
  take a connection for a moment, which then goes **through the link** (never directly); after
  that the name stops resolving (DNSRR at zero). The test checks exactly that.
- Not covered: a task on a **worker** node (the local suite is one node); the same path, the
  sidecar on a manager reached over the overlay, is unverified there. The "krill (notifications)"
  row of the spec's page sketch is KRL-003.

Plan: migration and queries → rollback gate (`selfupdate`) → SNI parser, egress stream
protocol, relay checks → docker aliases and egress-network inspection → deploy section,
networks and egress hook → sidecar spec, dynamic config, reconciler, app identification and
the client's egress path → handlers, pages, i18n → wiring → docs → integration tests. Lasting
rules: `networking.md#krill-link`, plus the AGENTS.md lines in the report.
