# Auth, tenancy and secrets

Sessions, roles, the instance-operator gate, password changes, and how secrets are stored and shown.

## Rules

### Every app/db route must chain-check tenant ownership and 404 on mismatch

Load org → project → environment (and db) and verify each FK matches its parent (`loadOrg` → `loadProject` → `loadEnvironment` → `loadApp`/`loadDBChain`). Mismatch returns `http.NotFound`. There are IDOR regression tests; keep them green.

### RBAC is owner > admin > member

**RBAC is owner > admin > member** (`Role` int with `RoleMember`/`RoleAdmin`/`RoleOwner` and `AtLeast`), enforced by `RequireRole(min)` middleware (403 if role < min). Admin+ can manage projects/apps/dbs/members; members are read-only.

### Instance-operator vs org-admin (security-critical)

Org-scoped `RoleAdmin` is **self-grantable** — any authenticated user can `POST /orgs` and becomes the new org's `owner`. So **global infrastructure (cluster nodes, the host-wide monitoring view) is gated by `RequireInstanceAdmin`**, backed by the instance-level flag `users.is_admin` (migration 000028), NOT by org-scoped RoleAdmin. Only the seeded `KRILL_ADMIN_EMAIL` user is promoted to `is_admin=true` on startup (`SeedAdmin`, idempotent); everyone else defaults false. `WithInstanceAdmin` loads the flag once per authed request into the ctx; `RequireInstanceAdmin` 404s non-operators (no capability disclosure). Do NOT gate a global/cross-tenant resource on `RequireRole` — use `RequireInstanceAdmin`.

### Only an owner may grant the owner role

**Only an owner may grant the owner role** (`updateMemberRole` in `internal/server/org_handlers.go`) — prevents admin self-escalation.

### App mutations and DB secrets are admin-gated

**App mutations and DB secrets are admin-gated** (2026-06-06 hardening): the app POST routes (deploy/rebuild/reload/stop/env/advanced) and the DB backup download/objects routes sit inside a `RequireRole(RoleAdmin)` group in `server.go`; plaintext connection-string display is admin-only too. Members are read-only viewers — no deploys, no DB dumps/creds.

### Login hardening

The unknown-email path runs a dummy bcrypt (`auth.dummyHash`) so it costs the same as a real check (no user-enumeration timing oracle); `POST /login` is per-source-IP rate-limited (`internal/server/ratelimit.go`, 10/min).

### Temp passwords flow via HttpOnly flash cookie

**Temp passwords flow via HttpOnly flash cookie** (`krill_flash_pw`), never in the URL; the template reads it once then clears it.

## Password changes

any authenticated user can change their own password from the "Change password" link in the sidebar of every organization page, which opens `/orgs/{id}/account/password` inside the normal layout (`internal/auth.Service.ChangePassword`: verifies the current password, enforces a 10-rune minimum, hashes and stores the new one, and revokes every *other* session for that account so a password change actually locks out anyone else who had a live session). `users.must_change_password` (migration 000041) is set on a user the first time they're added to any organization (`createMember`, new accounts only — never on an existing user added to a second org; written by the same `CreateInvitedUser` INSERT that creates the account, so an invite can never succeed without it) and gates every authenticated route except the standalone `/account/password` form (the `Bare` layout, no navigation — a non-forced user landing there is redirected to the in-layout page of their first organization) via the `requirePasswordChange` middleware, which fails **closed**: a lookup error holds the user on the form rather than letting a transient DB blip through to the rest of the app.

## Secrets & i18n

secrets are encrypted via `internal/secret` (`secret.Enc` on write, `secret.Dec` on read) at the store boundaries (dbservice store, db/registry/destination handlers, deploy store, backup store). UI masks them: DB connection strings render `type=password` + eye-reveal (`krillReveal`); env values are masked by default with a Show/Hide toggle (`krillEnvRevealToggle`). The UI is bilingual: `internal/web/i18n` has `en` + `ru` catalogs (keep both in sync — same keys); locale comes from the `krill_lang` cookie (set by `POST /lang` via the Settings language selector); `i18n.T`/`i18n.Tf` (printf) look it up.

## Pitfalls

### `secret.Dec` fails loudly

*(2026-08-05)*

It returns `(string, error)`; a value carrying the `enc:v1:` tag that cannot be opened (missing, rotated or wrong `KRILL_SECRET_KEY`) yields `("", secret.ErrUndecryptable)` — never the stored ciphertext. Credentials feeding a real operation (deploy, backup, DB instance, bot token, node SSH key) fail the operation; display paths log and render nothing. Consequence on a live install whose key was rotated: those paths now break *visibly* instead of shipping a bogus password. Never reintroduce a `, _ :=` at a `secret.Dec` call.
