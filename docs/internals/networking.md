# Networking, gateway and domains

How organizations get their own overlay networks, how Traefik routes apps and the Krill UI itself, and how domains, paths, redirects, protection and raw ports work.

## Per-organization networks

every organization now gets its own Docker overlay network, `krill-org-<id>` (`internal/orgnet.Name`), instead of sharing the single `KRILL_NETWORK`. `organizations.network_name` (migration 000042) records it once created; `organizations.network_migrated_at` (migration 000043) records that every one of the organization's services has actually been moved there — `network_name` alone can't carry that, since it is written *before* anything moves. `internal/deploy` (`GetOrganizationNetworkByApp`) and `internal/dbservice` (`GetOrgNetwork`) resolve an app's/instance's org network at deploy time and fall back to `KRILL_NETWORK` when it is still empty. Traefik joins every organization's network plus the base one (`traefik.Reconcile`, replacing the old one-shot `Bootstrap`; idempotent via a spec-fingerprint label so an unchanged spec doesn't force a task recreation on every restart) and is kept in sync as organizations are created (`Server.reconcileGateway`, a `func()`-typed field set by `SetGatewayReconcile` and triggered from `createOrg`, debounced 3s + rate-limited to one reconcile/minute so a burst of org creation can't be used to blip ingress repeatedly). On an existing install, `internal/orgnet.Migrator` runs once in the background **after** the HTTP listener is up (never blocks boot — a full pass can take minutes) and only once Traefik's own reconcile has succeeded (migrating services into networks the gateway never joined would be worse than not migrating): it moves each organization's databases before its apps (an app landing on the new network before its database can't resolve it by name), waits for each batch of databases to actually come up before enqueuing the apps that depend on them (a transient `ServiceStates` error is logged and polled past, not fatal), skips apps that are stopped or were never deployed, and **parks** stopped database instances instead of redeploying them (`orgnet.RunningInstancesFilter` + `dbservice.ParkInstance`: the service is rewritten onto the org network at its current replica count — zero for a stopped one — without starting it or waiting on it; skipping it would not do, because `StartInstance` only scales the existing service and would bring it back up on the shared network), and marks an organization migrated only once every step of its pass has completed — including every app deployment it submitted reaching a terminal status other than `error` (`RedeployApp` returns the deployment id and the migrator polls `ListDeploymentStatuses`; a submitted deploy proves nothing, and one that fails leaves that app on the old network). A partial pass (a failed database listing, a redeploy that errors, an app deployment that fails or is still unfinished when the 30-minute pass deadline expires) leaves the organization unmarked and the *whole* pass is retried on the next boot, not resumed piecemeal. The pass runs in two phases (2026-09-22): first every organization's databases are moved and its app redeploys submitted, then all organizations' deployments are polled together and each organization is marked as soon as its own finish — so one slow organization no longer spends the shared deadline while the ones after it wait unsubmitted; after the deadline one last sweep on a detached context (`finalSweepTimeout`) still records the organizations that did finish, and the mark itself is written on a detached context too. The database phase is still sequential per organization (each batch waits up to 3 minutes for its instances). Moving a service is an ordinary redeploy (`Deployer.EnqueueSystem`, bypassing both the per-app and per-org in-flight caps so the migration itself is never rate-limited by them), so a Dockerfile app is rebuilt (from cache, not `--no-cache`) to move networks — the first start after upgrading an install with several such apps is correspondingly slow. `docker.Engine.NetworkRemove` exists (idempotent, modeled on `NetworkEnsure`) but has no production caller yet — there is no "delete organization" endpoint in the codebase for it to clean up after.

## Panel domain

the UI is a host process, so the swarm provider (labels) cannot route to it; instead Traefik's **HTTP provider** polls Krill at `GET /_krill/gateway/config` (`panel.ProviderPath`) for the UI's routes (`panel.DynamicConfig`: `krill-panel-https` on websecure with `tls.certResolver=le`, `krill-panel-http` redirect-only on web, service → `http://KRILL_ADVERTISE_ADDR:<listen port>` from `panel.Upstream`; routers carry priority 1 000 000 so a tenant app claiming the same host can't tie; with an IP allowlist a higher-priority `krill-panel-machine` router keeps `/webhooks/`, `/api/`, `/mcp` unfiltered). Chosen over a file provider (a bind mount ties Traefik to a host path and breaks on Colima) and over running Krill as a Swarm service (changes the install model) — same dynamic-config shape Dokploy/Coolify write to files. Singleton table `panel_gateway` (migration 000045) holds a random `secret` (`secret.Enc`, created by `panel.EnsureSecret` at startup, replaced if undecryptable) from which **two** HMAC tokens derive (`panel.DeriveTokens`): the *provider* token goes into Traefik's args (`--providers.http.headers.X-Krill-Gateway-Provider=…`; args depend only on config + secret, so the spec fingerprint is stable and `traefik.Reconcile`/`TraefikSpec` take a `PanelProvider` — zero value = provider omitted when `panel.Upstream` fails, e.g. no advertise addr or loopback listen), and the *forwarded* token is stamped by a `headers.customRequestHeaders` middleware on every proxied request (`X-Krill-Gateway`). They must differ — the forwarded token rides on every request through the domain, and `gatewayConfig` 404s any request carrying it. A valid forwarded token is how the server recognizes a gateway request (`Server.viaGateway`/`secureViaGateway` + Traefik's own `X-Forwarded-Proto`, which Traefik strips from untrusted clients): it makes `X-Forwarded-For` trusted for the login limiter (`trustForwardedFor`, per request — `KRILL_TRUST_PROXY` stays for external proxies) and cookies `Secure` (`cookieSecure(r)`; `setFlash`/`setFlashPw`/`setFlashToken` now take `r`), so direct `http://ip:8080` keeps plain cookies and remains the way back. The login page warns when `KRILL_COOKIE_SECURE=true` meets plain HTTP. State `off → pending → active`: saving a host routes it immediately but changes nothing for the direct address; `confirmPanelDomain` accepts only a request on the domain over HTTPS (`onPanelDomain`) **and** a trusted cert (`panel.CheckCertificate` dials `ADVERTISE:443` with SNI — checks the cert only, not reachability; skipped with `KRILL_ACME_STAGING`). **Direct port close** (`closePanelDirectPort`, requires active + on-domain + lockdown already on) rebuilds the control-plane ruleset with `controlPlaneServicePorts(listen, closeDirect=true)` → UI port moves from the public set to `iifname "docker_gwbridge"` (`firewall.BuildManagerRuleset(ips, public, gateway)`), applied via `lockdownControlPlane` with the dead-man switch; `direct_port_close_pending` until `confirmPanelDirectPort` arrives through the domain. `lockdownControlPlane` now drops idle keep-alive conns after Apply (`SetIdleConnCloser` → `srv.SetKeepAlivesEnabled(false/true)`) so a pre-apply connection can't carry the confirmation. The page compares the stored flag with Krill's OWN table only (`directPortDrift` → `firewall.GatewayOnly`); when `inet krill` is missing altogether (msk: the port closed by a hand-made table) it says so (`panel.direct_drift_no_table`) and offers **Record as open** — its own route, `recordPanelDirectPortOpen` (`/panel-domain/direct-port/record`, so a form left open across a rollback reaches no handler that could apply a ruleset), which under `fwMu` requires `firewall.TableAbsent` (a positive `nft list tables` without `inet krill`; Status cannot tell that from an nft error) and only clears the flags (2026-11-25). Coupling with the Firewall page: a lockdown honors `direct_port_closed` and is refused off-domain when it is set; the firewall confirm also finalizes a pending panel close; **Open cluster** resets the flags; the page clears a close whose switch fired. Guards: the domain can't be changed/removed while direct access is closed or closing; apps can't take the panel host (`hostReservedByPanel` in `createApp`/`addDomain`) nor the panel an app's host; an operator on the domain can't save an allowlist excluding their own IP. **HSTS** (migration 000046, `panel_gateway.hsts_max_age`, CHECK: non-zero only while `active`; reset by `SetPanelDomainPending`/`DisablePanelDomain`): the global `panelHSTS` middleware sets `Strict-Transport-Security: max-age=N` only when `secureViaGateway(r)` AND state active AND `RequestHost(r)==host` — `max-age=0` while off, to unwind an earlier policy; never `includeSubDomains`/`preload` (the panel shares a parent domain with unrelated hosts); max-age is limited to `panel.HSTSPresets` (0/5m/1d/1w/30d/1y), set via `setPanelHSTS` (refused unless active). `TestPanelCookiesSecureOnlyViaGateway` pins the per-request Secure decision for both session and flash cookies (direct, direct claiming https, gateway https, gateway http, forged token). `Server.baseURL` prefers `KRILL_PUBLIC_URL`, then `https://<active panel host>`. No basic-auth option (it would eat the `Authorization: Bearer` of API/MCP). Rollback runbook (SSH: `nft delete table inet krill` + `UPDATE panel_gateway …`) lives in `docs/guides/domains.md`. Handlers: `internal/server/panel_handlers.go`, instance-admin routes under `/orgs/{id}/panel-domain`.

## Domains/TLS

*(Phase 5)*

the `domains` table (host UNIQUE, tls, is_primary) is the routing source of truth; `applications.domain` is the denormalized primary. `traefik.AppLabels(serviceName, []traefik.Domain, port, network)` emits per-domain routers — non-TLS on `web`, TLS on `websecure` with `tls.certresolver=le` plus a `web`→HTTPS redirect router (no global redirect). Domain CRUD re-applies labels via `engine.ServiceUpdateLabels` (service-level labels only → no container restart). Traefik bootstrap adds `:443` + the `le` HTTP-01 resolver + the persistent `krill-traefik-acme` volume at `/letsencrypt`. Real ACME issuance is verified manually (needs public DNS); locally TLS stays off and sslip.io domains serve plain HTTP.

## Gateway route exposure

per-domain `domains.exposed` + `domains.paths`. `traefik.AppLabels` skips non-exposed domains — **zero exposed → `traefik.enable=false` (overlay-only, internal)**; exposed + paths → `Host(...) && (PathPrefix(...) || …)` rule (anything else 404); `validPaths` guards against rule injection. **New apps are internal by default** (auto primary domain `exposed=false`, with a "not exposed" warning banner); existing domains were backfilled `exposed=true`. UI: Domains tab has a per-domain Exposed toggle + paths editor. The "who" axis (basic-auth / IP-allowlist) is done — see [Route protection](#route-protection-basic-auth--ip-allowlist) below.

## Route protection (basic-auth / IP-allowlist)

per-domain `domains.basic_auth_users` (htpasswd `user:bcrypthash`, newline-separated) + `domains.allowed_ips` (CIDR/IP, newline-separated; migration 000020). `traefik.AppLabels` attaches middlewares to the serving router (non-TLS `base`, TLS `base+"s"`; the redirect router is left unprotected) in order `ipallowlist.sourcerange` then `basicauth.users` — chained = AND. Admin-gated handlers (`addDomainBasicAuthUser`/`deleteDomainBasicAuthUser`/`setDomainAllowedIPs`) validate username-regex + `net.ParseCIDR`/`ParseIP` (canonicalized), bcrypt via `auth.HashPassword`; passwords/hashes are never logged or shown — logins only. UI: "Access protection" section on the Domains tab (exposed domains only). IDOR/admin-gate tests + a 4-lens adversarial review are green.

## Shared hosts by path

*(2026-10-02)*

implemented on `feat/shared-host-paths`; live
Let's Encrypt acceptance remains in ROADMAP. Apps of one environment can share a host,
with one catch-all and disjoint segment paths. `internal/hostshare.Store` checks and writes
under a per-host PostgreSQL advisory lock; `Add` inherits the host's TLS, `UpdatePaths`
checks its own row without self-conflicts, and `SetTLS` requires the original domain ID
still to exist before changing all rows. `CreateApplication` reserves an exclusive primary
host and inserts app + domain atomically; `SetPanelDomain` uses the same host lock, and
app writes recheck the panel reservation inside it. All domain mutations must preserve
this lock protocol now that `domains_host_key` is gone. Domain claims are row-locked to
prevent deletion during a checked update. The Domains tab lists sibling apps and confirms
host-wide HTTPS; topology ingress labels include paths. See `docs/guides/domains.md`.

## Domain redirect to the primary domain

*(2026-11-16)*

branch `domain-redirect-to-primary`;
live Let's Encrypt acceptance remains in ROADMAP. `domains.redirect_to_primary` (migration
000060, CHECK `NOT (is_primary AND redirect_to_primary)`) makes a non-primary domain answer
every request with a `redirectregex` middleware (`^https?://[^/]+(.*)$` →
`<primary scheme>://<primary host>${1}`, `permanent=true`: Traefik v3.6 sends 301 to GET and
308 to every other method — it has no 308-on-GET option). `traefik.AppLabels` computes the
target from the `Primary` row on every label write, so a primary change moves redirects with
the next label sync; redirect routers are Host-only at catch-all priority (no paths, hidden
metrics paths, basic-auth or allowlist), routed regardless of `exposed`, TLS gets its own
`le` certificate on websecure plus a web router redirecting straight to the target (ACME's
`acme-http@internal` router has priority `math.MaxInt`, so HTTP-01 is never redirected). A
redirect without a primary in the list is not routed. Every label writer converts rows with
`deploy.TraefikDomains` — add new routing fields there. `hostshare.Store.SetRedirect`/
`MakePrimary` run under the per-host lock: a redirect takes its whole host (`Claim.Redirect`
in `Check`, `flash.err.host_redirects`), is refused on the primary (conditional UPDATE + the
CHECK), and **Make primary** (the first way to change `is_primary` after creation) is refused
for a redirecting domain and for a host other apps share, rewrites `is_primary` for every row
of the app and `applications.domain`; it takes an application advisory lock
(`krill.domain-primary:<appID>`) BEFORE the host lock — two primary changes on different
hosts would otherwise deadlock on each other's target row (`TestStoreConcurrentMakePrimary`),
and then locks the `applications` row before any `domains` row (`LockApplicationRow`) — an app
delete takes them in that order via ON DELETE CASCADE (`TestStoreMakePrimaryVersusApplicationDelete`).
Since the primary can now move, `deleteDomain` deletes with `DeleteNonPrimaryDomain`
(`AND NOT is_primary`, 0 rows ⇒ refused), and `CountExposedDomainsByApplication` ignores
redirecting rows. Traefik drops routers of a service without running
tasks (`allowEmptyServices=false`), so a stopped app's redirect 404s — documented, not changed.

## Raw TCP/UDP published ports

`app_ports` table (migration 000025: `host_port`/`container_port`/`protocol`, `UNIQUE(host_port, protocol)`) → `deploy.App.Ports` → `spec.Ports` (host publish mode; the `internal/docker` layer already supported TCP/UDP via `PortSpec.UDP`). Admin-gated `addAppPort`/`deleteAppPort` validate range 1..65535 + protocol ∈ {tcp,udp} and conflict-check the host port against both `app_ports` AND managed-DB `external_port` (for TCP); tenant chain-check + IDOR. UI: "Published ports" on the Advanced tab. Applied on the next deploy. Deferred: ingress (swarm LB) mode, port ranges, auto-pick, firewall/UFW.

## Krill link

*(2026-11-27, KRL-001: the core; 2026-11-28, KRL-002: egress; ingress KRL-005)*

Two installations linked: a **client** holds one outbound TLS 1.3 connection with mutual
authentication to a **relay**, and both sides multiplex streams over it with
`github.com/hashicorp/yamux`. One role per install (`link_settings.role`: `off` | `client` |
`relay`), set only by the instance operator (`RequireInstanceAdmin`, Settings → Link,
`/orgs/{id}/link`). Package `internal/link`; the operator page in
`internal/server/link_handlers.go` + `templates/link.templ`; user guide `docs/guides/link.md`.

**Data (migration 000066, additive).** `link_settings` (singleton; gateway/listener certs for
both roles; relay: `link_host`, CA, server cert; client: relay address, SNI, CA fingerprint,
client key and cert), `link_peers` (relay; `name` and `cert_sha256` UNIQUE,
`prev_cert_sha256` for renewal), `link_pairing_codes` (`secret_sha256` UNIQUE, `expires_at`,
`used_at`, `key_sha256`, `issued_cert`) and `link_ingress_hosts` (empty until KRL-005, created
now so the host reservation survives a rollback from stage 2). Every private key goes through
`secret.Enc` in `link.Store`; a pairing secret is stored only as its SHA-256; nothing in these
tables is a live bearer credential, so `statebackup.DumpArgs()` is unchanged (a state restore on
another host clones the client's identity: the relay then refuses the second session as a
duplicate). Code expiry uses the database clock (`now()` in `CreateLinkPairingCode` and
`LockLinkPairingCode`), never the host's.

**Transport.** The client pins the relay by the SHA-256 of the link CA from the pairing code
(`pinnedTLS`/`verifyPinned`: the server sends `[server, CA]`, the leaf must chain to that CA for
the SNI); system roots play no part. The relay asks for a client certificate
(`VerifyClientCertIfGiven` against its CA) and, after the handshake, looks the certificate's
fingerprint up in `link_peers` on every connection — so removing a peer revokes it at once. The
first stream decides: without a certificate only one `pair` stream is accepted; with one, a
`control` stream whose header is `hello {proto, krill_version}`. The relay answers a JSON line
`hello` or `error{category}`, so the client shows `unauthorized`/`duplicate`/`incompatible`
rather than a bare TLS alert. Every stream starts with a header (1-byte type, 2-byte length,
JSON; `MaxHeader` 4 KB, `HeaderTimeout` 5 s); an unknown or not-yet-supported type is closed
without reading further (yamux v0.1.2 has no public RST) and logged at WARN; at most
`MaxStreamsPerPeer` (1024) per peer, counted with yamux's own `NumStreams` — a stream closed on
one side stays in the session until the other side closes it or `StreamCloseTimeout` (30 s)
resets it, so a peer over the limit loses its whole session (both sides). **Before admission a
connection may send at most 64 KiB** (`budgetConn`): the relay's TLS accepts pairing without a
certificate, and yamux buffers up to 256 KiB per stream that is not read yet, so without the
budget an unauthenticated peer could make the relay buffer that much per stream. yamux keepalive 15 s with `ConnectionWriteTimeout` 30 s,
so an unanswered ping closes the session 45 s after the last answered one. The client
reconnects with backoff 1–60 s, jittered over [d/2, d) (`nextDelay`, `jitter`); every `Wake`
(pairing, Forget) bumps a generation, and an attempt that read the settings before it is
dropped instead of becoming the session. Certificates are renewed over the live session at two
thirds of their lifetime, counted from issue (`needsRenewal`; `NotBefore` is backdated an hour
for clock skew): `renew{csr}` → `cert`; the certificate the session holds stays valid as
`prev_cert_sha256` until the first connect with the new one (`MarkLinkPeerConnected` clears
it). **A session renews once and then reconnects with the new certificate**: a second renewal
while it still ran on the first would retire the stored one in favour of one whose answer might
be lost, and a session left running could outlive both. A session whose answer was lost may ask
again (the relay keeps the session's certificate as previous). The relay records the connect
(`MarkLinkPeerConnected`) before it answers `hello`, and refuses the session when it cannot. The
relay's server certificate is reissued under the same CA 90 days before it expires
(`renewServerIfDueReport`, at start and daily, and whenever the relay is turned on): under the
host lock it re-reads the settings and only replaces the certificate (`SetLinkServerCert`) while
they still name this relay and host, so it never undoes a role change.

**Admission and revocation are serialized per peer ID** (`registry`): a session is registered
under the peer's lock after re-reading the peer by ID (and its fingerprint) and checking that
the relay configuration that accepted the connection is still the running one; `RemovePeer`
deletes the row and closes the live session under the same lock; `unload` (role off, shutdown)
stops serving first and then closes every registered session, so nothing admitted in between
survives; every new stream checks its session is still the registered one. The relay keeps the
latest refused session per peer (`Refusal`: duplicate or incompatible, with the address) for the
page. A pairing checks the running relay and commits under `Relay.gate`, which `unload` takes
before it stops serving, so no code is redeemed after the relay was turned off. `revoke` closes
the peer's session whatever the delete reports (an answer lost after the commit). Operator
changes — role, pairing, Forget — are serialized (`Service.opMu`), so a pairing that answers
late cannot undo a role change or a Forget; a stored role change is applied on a context
detached from the request (`applyAfterWrite`), and `SetOff` stops everything without reading the
settings back. At start a stored role whose listener port turned public is not listened on. One live session per peer: a second attempt pings the first (5 s) and
is refused as `duplicate` while the first answers (shown on the page with its source address —
usually a clone restored from a state backup); a first session that does not answer is closed
and replaced, so a client whose old TCP connection died silently does not wait 45 s.

**Pairing.** `krl1_<base64url(JSON{addr, sni, ca_sha256, secret})>`, created by the relay
operator for a peer name, shown once through the HttpOnly `krill_flash_link_code` cookie, valid
10 minutes and single-use. The client generates its key, dials pinned to the CA without a
certificate and sends `{secret, csr}` on a `pair` stream. `Store.Redeem` is one transaction: the
code row is locked (`FOR UPDATE`), then a conditional UPDATE (`used_at IS NULL AND expires_at >
now()`) records the CSR key's SHA-256 and the issued certificate (CN = peer name, one year) and
the peer is created. A retry with the **same key** before expiry gets the same certificate (the
answer can be lost after the commit; `Client.Pair` retries once on a transport failure and keeps
the key of an attempt that was never stored, by the code's hash, for a later submit of the same
code; dropped only on a final refusal, not on a rate limit or a relay error — the key lives in
memory, so a restart in between still means removing the peer and pairing again), any other key
is refused. Expiry is checked by a separate query after the row lock is held
(`LinkPairingCodeLive`, `clock_timestamp()`): the lock can be granted after the code expired,
`now()` is the start of the transaction, and an expression in the locking query is evaluated
before the wait.
Categories and types the other side sends (pairing and session refusals, renewal answers,
unknown control messages) are matched against the known ones before they are shown or logged. Pairing attempts are limited to 10/min per source address — the address
from the PROXY header, which is trustworthy only because of the next paragraph.

### The link listener accepts only PROXY v2 + mTLS with the gateway's certificate

`KRILL_LINK_LISTEN_ADDR` (default `:8445`) is listened on while the role is not `off`
(`link.GatewayListener`); a port the lockdown keeps public (22, 80, 443, the UI port) is refused
for either role (`ListenRequirement`). At most `maxPending` (256) connections are not yet
admitted: a slot is held from accept through the inner TLS until the relay admits, pairs or
refuses the client. Any container on the host can reach it and forge a PROXY header, so
each connection must send a PROXY v2 header (`ReadProxyHeader`: PROXY command, TCP over IPv4/6,
body ≤ 512 bytes; fuzzed) and then complete TLS presenting exactly the gateway's client
certificate (`RequireAnyClientCert` + fingerprint pin); it is closed before its inner stream is
read otherwise, and the header is believed only together with that handshake. The gateway's
client certificate and the listener's certificate are self-signed, created with the role
(`EnsureLocalMaterial`, replaced when they cannot be decrypted) and pinned on the other side:
Traefik gets the listener's certificate as its `rootCAs` for `serverName`
`krill-link-listener`. On a client the listener serves the egress sidecar's connections (below).

### The relay's endpoint is a TCP route merged into `/_krill/gateway/config`

`gatewayConfig` merges `panel.DynamicConfig` (`{}` while the panel domain is off) with
`link.Service.GatewayTCP`: router `krill-link` `HostSNI(<link host>)` on `websecure` with
`tls.passthrough`, service → `KRILL_ADVERTISE_ADDR:<link port>` with `tls: true` through
serversTransport `krill-link-gateway` (`proxyProtocol.version=2`, `tls.serverName`,
`rootCAs`, inline `certificates` of the gateway — the provider's answer is behind its token).
No middlewares: TLS is end to end between the client and Krill. The panel's HTTP routers keep
working beside it (TCP routers are tried first on an entrypoint). The relay role needs what
the panel domain needs (`link.Upstream` → `panel.Upstream`: an advertise address, a non-loopback
UI listener) and a non-loopback link listener, not the panel domain itself.
`TestRelayGatewayOnRealTraefik` (`-tags=integration`) runs the pinned Traefik with the HTTP
provider and pairs a client through it.

### The link host and ingress hosts are reserved under the host lock, in both directions

`hostshare.Reserved` (no lock, for handler pre-checks with a reason: `flash.err.host_reserved_panel`
/ `_link`) and `reservedErr` (under `krill.domain-host:<host>`, called by `Add`,
`ReservePrimaryHost`, `CreateApplication*`, `MakePrimary`, and so by environment cloning; the
host lock key is lower-cased, and the link's own comparisons ignore case as DNS does) check
the panel domain, `link_settings.link_host` while the role is relay, and `link_ingress_hosts`; it
replaced `panelAvailable`. `SetPanelDomain` checks the link hosts; the link host is written by
`Store.ReserveLinkHost` under the same lock, refused while a domain row or the panel holds it. A
release without the link does not know the reservation, so after a rollback a domain can sit on
the link host: **before every publication** `GatewayTCP` re-checks under the host lock
(`LinkHostHolders`: the panel, domain rows and `link_ingress_hosts`) and publishes nothing while
anything else holds it, building the route from the settings re-read under that lock; the Link
page lists the holders (panel, ingress host, or `project/env/app` with a link to its domains tab)
until the operator removes one. `gatewayConfig` also drops the route when the panel row it read
before that check names the same host, so one answer never routes a host both ways. The link
host's key material is chosen inside the same transaction under the settings row lock
(`writeRelay`), so two first enablements cannot each create a CA.

### The link port is gateway-only under lockdown, and Krill never re-applies the firewall

`controlPlaneServicePorts(listen, closeDirect, linkPort)` adds the link port to the `Gateway`
set (`iifname "docker_gwbridge"`) while the role is not `off`, never to the public one. Krill does
not apply the firewall on a role change (the dead-man switch needs the operator's confirmation):
the Link page compares the live ruleset's marker with the one it needs now
(`linkFirewallStale`) and asks to re-apply with a link to the Firewall page, whose drift check
shows the same. Since the gateway-only rule can now carry the link port alone, the panel
domain's direct-port drift asks whether **the UI port itself** is gateway-only
(`portGatewayOnly`), not whether any port is (`firewall.GatewayOnly`, now without a production
caller).

### Egress (KRL-002)

**Data (migration 000067, additive).** `link_egress_served (application_id, host, port,
deploy_floor)` — the names the sidecar answers for (below); `link_destinations (host, port)` — the relay's one list,
the hard boundary for every peer; `link_org_grants (org_id)` — the client operator's grants;
`app_link_destinations (application_id, host, port)` — an app's saved selection (no FK to the
relay's list: it lives on another installation; a destination the relay dropped stays selected and
is refused). Hosts go through `panel.NormalizeHost` (`link.ParseDestination`), ports
`strconv.Atoi` 1..65535. Queries in `queries/link_egress.sql`.

**Protocol.** Both `hello`s carry `egress: true`; only then does the client send `manifest`
(services: app id, `project/env/app`, destinations — for display only) and the relay send
`lists` (its destinations, also in its `hello`, only to a client that said egress). The relay
writes its `hello` and every `lists` under the session's write lock, reading the list there:
nothing precedes the `hello`, and a later push never sends an older list. The list is bounded
(`MaxDestinations` = 50, checked under the settings row lock) so it always fits one control line
(16 KiB). On a client one app holds at most 256 egress streams and all apps together the
session's limit minus 64, so one app cannot make the relay close the shared session. A spliced
connection is closed when its session ends (a stream of an ended session reads as EOF, and the
destination may keep its half open). A KRL-001 relay closes the session on an
unknown control message, so neither side sends the new ones to a peer that did not announce
them. An `egress` stream's header is `{host, port, service}`; `service` labels the counters and
pages, it is never a permission.

**Relay checks** (`Relay.serveEgress`), in order, each refusal a WARN with peer, service and
`host:port`, before anything is dialed: `{host, port}` is exactly normalized and on
`link_destinations` (read per stream, so a removal applies to the next stream); the stream's
ClientHello (`ReadClientHello`, fuzzed: TLS records, multi-record hello, ≤ 64 KiB, nothing read
past it) names exactly `host` — no SNI is refused, and so is Encrypted Client Hello (`ErrECH`),
whose visible name is only the front end's — so a compromised client cannot reach another
virtual host behind an allowed name's shared TLS front end; then the relay resolves and dials
through `netguard.DialContext(false)` (10 s): a name resolving to a private, loopback or
link-local address is refused (DNS rebinding towards the relay's own services). The buffered
ClientHello is written on and both directions are spliced (`splice`, half-close per direction).
The guarantee stops at the TLS name: an HTTPS front end routing by the HTTP `Host` can still be
asked for another of its sites (the guide says so). A failed read of the list ends a session's
`hello` (the client reconnects) but never a working session on a later `lists`.

**Client path.** The listener hands the whole PROXY header to the role (`dispatch`): on a client
the connection is the sidecar's, carrying an app's TLS stream. `Egress.serveConn`: identify the
app (`identify`: the PROXY **destination** is the sidecar's task address on the app's own egress
network, the **source** must be a task of `krill-<appID>` on that network; addresses come from
`docker.PrefixNetworks`, cached; a miss refreshes once, at most every 250 ms, then refuses);
read the ClientHello's SNI itself; then `decideEgress` — session up ∧ organization granted ∧
`(SNI, PROXY port)` in the app's **saved** selection ∧ on the relay's last list. The grant and
selection are read from the database for every connection (`LinkEgressDecision`), so revoking a
grant or a destination closes new connections at once, without a redeploy. A refusal resets the
connection (linger 0) and logs a WARN with app id, `host:port` and the reason, never app data.

**Per-app networks and the sidecar.** An app whose *applied* selection is not empty gets its own
overlay `krill-egress-<appID>` (MTU as `orgnet`, through `NetworkEnsure`) as a second network
(`buildSpec`: `Networks = [org network, egress network]`); Traefik still routes it over the org
network through `traefik.docker.network` — never add a `traefik.swarm.*` label beside it, Traefik
v3.6.1 skips a service carrying both kinds (`TestEgressAppAnswersThroughTraefik` proves the label
wins over a provider default Traefik cannot reach). The applied selection is the saved one while
the role is `client` and the organization is granted (`ListAppLinkEgress`), otherwise empty: a
revoke or role off marks the apps "settings not applied" and their next deploy leaves the
network. The egress release's deployer therefore manages that attachment itself and no longer
sets `KeepNetworkPrefix` (the core release's deployer still does, which keeps a rollback onto it
fail closed). Before such a deploy `EgressHook.Prepare` creates the network and has the sidecar
answer for the app's hosts there, waiting for its new task; a failure fails the deploy.
`krill-link-egress` is the pinned Traefik on `node.role==manager`, **DNSRR** (an alias resolves to
the task's own address, so Krill sees the app's task address; a VIP would replace it with the
network's balancer), no docker socket, no swarm provider, **two replicas**. With no task at all
Docker's embedded DNS stops answering for the aliases and forwards the name upstream: the app gets
the real address and its traffic leaves directly — measured on Docker 29.5.2 for DNSRR (at once)
and VIP (after ~10 s) alike, with a name that really resolves (`api.telegram.org`; the design
note's check used `example.com`, which resolved to nothing there, and read as fail closed). So
Krill never scales the sidecar down itself, runs two tasks so one failing leaves the names
answered, and every reconcile redeploys it when fewer tasks run than it should (ERROR in the
log). A manual scale to zero or both tasks down is a fail-open window until the next reconcile
(at most a minute); the user guide says not to. Its aliases on each network (`docker.ServiceSpec.NetworkAliases`, omitempty) are
the app's **saved ∪ applied ∪ current-spec ∪ preparing** selection: applied is the newest
successful deployment that recorded a link section (`ListLatestLinkApplied`; `applied_config.egress`
is plain `host:port` beside the digests, `deploy.SectionLink` the fingerprint section — a deploy by
the core release after a rollback records none and does not count as "nothing"); current-spec is
the app service's `krill.link.egress` label (`deploy.LinkEgressLabel`, set by `buildSpec`), which
covers a deploy not recorded yet and a restart of Krill in the middle of one; preparing is what a
`Prepare` in flight asked for (kept until a reconcile sees the app's service on its network, or
30 minutes); and every name the sidecar already answers for (`link_egress_served`, migration
000067, with the app's newest deployment id at the time — the floor). A served name is retired
only when none of the others needs it, every live task of the app runs the service's current
spec (`converged`: the task's `ForceUpdate` equals the service's) and the app's newest
link-recording deployment is above the floor; names are recorded before the sidecar answers for
them. So a restart in the middle of a deploy, a domain change rewriting the service's labels
(`ServiceUpdateLabels` replaces them) or a save undone before any deploy cannot drop a name an
old task may still resolve. A host leaves DNS only once a deploy without it has converged, and
until then its connections are refused, never resolved to the real address. A network counts as
used while a task of the app is still alive on it, a task Swarm is stopping included
(`PrefixNetworks` lists desired-state `shutdown` tasks that still run), so draining old tasks keep
their names. A fingerprint recorded before the link section counts as having applied no selection
(`withLinkDefault`), so a first selection after the update shows as not applied. One entrypoint per distinct
port; with none the `entryPoints` key is left out (an empty one stops Traefik). Its static
configuration — the HTTP provider `http://<advertise>:<ui port>/_krill/link/egress-config` and its
token, an HMAC of the panel secret under its own label (`EgressToken`) — is a Swarm secret named by
its content and passed as `--configFile`, never in argv or labels; the spec carries a fingerprint
label, so unchanged inputs do not recreate the task. The dynamic configuration routes, per port,
`HostSNI` of the saved destinations of granted organizations (only while the role is `client`)
with passthrough to `<advertise>:<link port>` through a transport with PROXY v2 and the
installation's gateway client certificate inline (on a client the main gateway never gets that
certificate); anything else has no route.

**Lifecycle** (`Egress.reconcile`, at start in any role, on every kick — role, grant, selection,
a finished deploy — and every minute): a network stays while any service other than the sidecar
attaches it or has a task on it, or while its app is being prepared; the sidecar leaves an unused
network and then it is removed; with none left the sidecar service is removed. Removing the
sidecar earlier would make the names resolve to the real addresses (fail open).

**Pages.** The relay lists its destinations and, under each peer, the services of its manifest
with bytes up/down; the client lists the relay's list as sent, the organizations with grant
controls and its services. The app's settings tab has a link card only while the link is open to
its organization (otherwise no card and the POST is 404), with the line that a host is
intercepted on every port. Counters are in memory since the process started.

**Rollback gate.** Granting an organization and saving an app's selection are refused, with
the reason, while `selfupdate.Updater.RollbackBinary` reports `krill.prev` older than
`link.CoreRelease` (`v0.17.0`, the first tag with the core) or unknown (path unresolved, updater
not wired, `--version` fails, times out or prints no `krill vX.Y.Z`); absent is allowed. The gate
does not revoke an existing grant.

**Limits.** TLS only, exact names, no wildcard, UDP or MTProto; interception is per host, not
per port; the pre-deploy one-off container is not identified as the app and does not use the
link. Krill's own notifications through the link are KRL-003.

### A redeploy keeps every `krill-egress-*` attachment

*(the link core release, v0.17.0 — the rollback target of the egress release)*

That release's `Deployer.buildSpec` sets `docker.ServiceSpec.KeepNetworkPrefix =
deploy.EgressNetworkPrefix` (`json:",omitempty"`); the egress release's deployer manages the
app's egress attachment itself and leaves it unset (see Egress above). `ServiceDeploy` resolves the running service's attachments to names
(`attachmentNames`, network IDs as Swarm stores them) and carries over those with the prefix,
aliases included (`keepAttachments`), unless the spec attaches that network itself. No such
network exists before KRL-002; afterwards this is what keeps a rollback from the egress release
fail closed — without the attachment and its aliases, the app's DNS returns the real address and
its traffic leaves directly. Hence KRL-002 ships in a later release than this core, and
`docs/install.md` Rollback says what going further back does.

**Logs** carry the peer name, the category and sizes, never keys, certificates, pairing codes or
stream bytes (TLS failures are logged as a category, not the peer's error text).

## Pitfalls

### Traefik is pinned to `v3.6.1`

**Traefik is pinned to `v3.6.1`** (`internal/traefik/bootstrap.go`, const `TraefikVersion`). Versions ≤3.6.0 hardcode Docker API 1.24, which Docker Engine 29.x rejects ("client version 1.24 is too old"), breaking the Swarm provider and all routing. v3.6.1 auto-negotiates the API version. Traefik is configured via CLI args only (no `traefik.yml`); the only mount is the docker socket (read-only).

### A host-mode published port deadlocks a StartFirst update

*(2026-09-13)*

The replacement task binds the same port on the same node, so Swarm leaves it `Pending` ("no suitable node (host-mode port conflict)") and — StartFirst — never stops the old task: the update neither finishes nor fails, the old spec keeps running, and nothing is logged. It hit Traefik (`:80`/`:443` in host mode, pinned to the manager): every gateway spec change after the first create (a new organization, the startup reconcile after an upgrade) silently left Traefik on its old networks. `buildSwarmSpec` now uses StopFirst for any service with a host-mode port (Traefik, raw TCP/UDP app ports, DB proxies — a brief gap instead of a hang), and `traefik.Reconcile` waits for the new task to run and for Swarm to mark the update `completed` (baseline-relative, 3 min — `completed` lands after the 5s update monitor, inside which Swarm can still roll back) and returns an error otherwise, so `gatewayReady` — and with it the org-network migration — means the gateway really joined the networks. Reconcile also refuses to trust a matching spec-hash label while the service's `UpdateState` is still `updating`/`paused`: the label is written with the spec, so an install stuck before the fix carries the current hash and would otherwise never be redeployed.

### Gateway timeouts are explicit and long responses are safe

*(2026-11-21)*

`traefik.gatewayTimeoutArgs` pins Traefik v3.6's own defaults (measured on the pinned image): entrypoint `readTimeout=60s` (reading the whole REQUEST, body included — slow uploads fail), `writeTimeout=0s` (no cap on a response), `idleTimeout=180s` (keep-alive idle BETWEEN requests only), and towards apps `dialTimeout=30s`, `responseHeaderTimeout=0s`, `idleConnTimeout=90s`. `TestGatewayTimeoutsAndAccessLogOnRealTraefik` (`-tags=integration`, ~90s) proves a response silent for 75s is delivered whole over HTTP/1.1 and HTTP/2; a 200s silence was also checked by hand. `KRILL_GATEWAY_ACCESS_LOG` (`off`/`filtered`/`all`, `traefik.SetAccessLog` before the first Reconcile) adds JSON access-log args: `filtered` = status 400-599 OR retried OR duration ≥ 10s (Traefik ORs filters), headers dropped, query strings logged. Any change to these args changes the spec fingerprint, so the first start after an upgrade redeploys the gateway once (stop-first: a few seconds without ingress).

### Shared hosts need explicit priorities

Metrics deny rules can make a catch-all's
rule longer than a path rule. App path routers use 200, catch-all 100 (both below the
panel's 1,000,000), including HTTP-to-HTTPS redirects. `/api` matches `Path(/api)` or
`PathPrefix(/api/)`, never the raw string prefix that also matches `/apiary`. Normalize
legacy trailing slashes and `/` on reads, not only when saving a domain.

### Hidden metrics paths use ``PathRegexp(`(?i)^<path>(?:[/;].*)?$`)``, not `Path`/`PathPrefix`

Case-insensitive backends (ASP.NET) and servlet containers stripping `;` parameters would otherwise serve `/METRICS` or `/metrics;x` through the domain. `internal/traefik`'s `TestHiddenMetricsPathsOnRealTraefik` (`-tags=integration`) runs the pinned Traefik and fails on the old rule.

### An overlay network's `com.docker.network.driver.mtu` is the underlay MTU, not the containers'

*(2026-11-06)*

Containers on the network get the option minus 50 (VXLAN); no option means 1500, hence Docker's 1450. `KRILL_OVERLAY_MTU` is the container-side value and `overlayCreateOptions` adds the 50 — verified by `TestOverlayNetworkMTU` (`-tags=integration`), which reads `eth0`'s MTU inside a container. An existing network's MTU cannot change; it applies only at creation. The nftables ruleset is persisted only on confirm (`/etc/krill/firewall.nft` + `krill-firewall.service`) and carries an empty `krill_rs_<hash>` chain as its identity, which the Firewall page compares live against the ruleset the cluster needs now.

### The firewall dead-man switch has one snapshot per node

*(2026-09-22)*

`firewall.Apply` snapshots the table to `/run/krill-fw-prev.nft` and arms the `krill-fw-revert` timer that restores it. A second Apply before Confirm used to overwrite the snapshot with the unconfirmed ruleset and only then fail on `systemd-run` (the unit name was taken), so the revert put back the change it was guarding. This was hit live by pressing **Close direct access** twice. Now one shell script (`armScript`) refuses while the timer or its service is active (`*firewall.RevertArmedError` carries the revert time), and only then snapshots and arms. The script runs under an `flock` on a fixed fd, so two overlapping Applies can't both pass the check. Every firewall-changing handler also holds `Server.fwMu` (`lockFirewall`, which refuses with "busy" instead of waiting), because they read the stored state, change the host and record the result as separate steps. The snapshot starts with the same `table inet krill { }` + `delete table inet krill` prologue as `buildRuleset`, since a bare `nft list table` restored with `nft -f` appends to the live table instead of replacing it. A ruleset `nft -f` rejects changed nothing, so Apply disarms the switch it just armed. Only `firewall.Open` (**Open cluster**) stops the timer without waiting. **`Confirm` (2026-11-07)** disarms only a timer that is still waiting (`disarmIfArmedScript`; otherwise `ErrRevertFired` and nothing is persisted — the revert may be restoring the previous table right then). `systemctl is-active` on the timer alone can't tell a waiting timer from one whose revert service already runs, and stopping the timer doesn't stop that service, so Confirm and the revert share one protocol: both take an `flock` on `<snapshot>.lock`, Confirm (under it) refuses while the service is active and otherwise leaves a `<snapshot>.confirmed` marker before stopping the timer, and the revert (`revertShell`, under the same lock) exits without touching nft when the marker exists, and otherwise leaves a `<snapshot>.reverted` marker before restoring, which makes Confirm answer "expired" — on real systemd a Confirm that waited for the lock while the revert ran then saw the service inactive and the timer still active, and confirmed a reverted change; `armScript` deletes both markers before arming, and arms the timer with `AccuracySec=1s` (systemd's default of one minute let it fire up to a minute after the revert time the page shows). `TestConfirmProtocolOnRealSystemd` (`-tags=realsystemd`, a privileged systemd container, see the test) checks this on systemd 252 and 257. Once disarmed, Confirm never returns an error for a failed persist (logged; the page's drift note shows it), because callers treat a Confirm error as "not confirmed" while the ruleset would be in force with no revert left. Removing a node opens its firewall (its persisted ruleset would otherwise reload at every boot with nothing in Krill able to open it), under `fwMu` through the Open and the row delete — a lockdown running meanwhile would otherwise apply its ruleset after Open stopped the revert. Swarm ports are bound to the WireGuard interface only with a complete address list (`swarmAddrSet`): a missing address makes binding MORE likely, so any doubt leaves it unbound.
