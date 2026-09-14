# Domains, HTTPS and access control

Every app gets one generated domain, `<name>.<KRILL_BASE_DOMAIN>`, and can have as many
custom domains as it needs. All of it is managed on the app's **Domains** tab and applied
through Traefik labels, without restarting the app.

## New apps are internal

A new app is **not** reachable from the internet. Its services on the organization's network
can call it by name, but Traefik routes nothing to it until a domain is marked **Exposed**. The
app page shows a warning while no domain is exposed.

## Add a custom domain with HTTPS

1. On the **Domains** tab, add the host, e.g. `bot.example.com`.
2. Point an `A` / `AAAA` record for it at the server's public IP.
3. Mark the domain **Exposed** and enable **HTTPS**.

Traefik obtains a Let's Encrypt certificate through the HTTP-01 challenge, serves the domain
on `:443` and redirects `http://` to `https://`. Certificates live in the `krill-traefik-acme`
volume, so restarts don't request them again and hit rate limits.

Issuance is asynchronous. If a certificate doesn't appear, check that DNS points at the
server, that ports 80 and 443 are reachable, and the `krill-traefik` service logs. While
testing, set `KRILL_ACME_STAGING=true` to use the staging CA. A domain without HTTPS enabled —
like the local sslip.io one — stays on plain HTTP and never triggers ACME.

## Expose only some paths

An exposed domain serves every path by default. To publish only part of an app — say, a bot's
webhook endpoint — list path prefixes under **Public paths**, one per line (`/webhook`).
Requests to any other path get a 404 from Traefik and never reach the app.

## Protect a domain

**Access protection** on an exposed domain adds two independent checks:

- **Basic auth** — one or more username/password pairs. Passwords are stored as bcrypt hashes
  and never shown again.
- **Allowed IPs / CIDRs** — one address or range per line, e.g. `203.0.113.4` or
  `10.0.0.0/8`.

When both are set, a request must pass both. Paths decide *what* is public; protection decides
*who* may reach it.

## Serve the Krill UI on a domain

Out of the box the UI answers on `http://<server-ip>:8080`, in plain HTTP — the admin password
travels in clear text on every sign-in. **Settings → Panel domain** (instance admin only) puts
the UI behind the same gateway and Let's Encrypt resolver your apps use, on a domain of its
own. Nothing has to be edited on the server.

**Before you start:** an `A`/`AAAA` record for the domain points at the control-plane server,
ports 80 and 443 are reachable from the internet, and `KRILL_ADVERTISE_ADDR` is set (the
installer writes it). The domain can't also belong to an app.

1. **Enter the domain and save.** The gateway starts routing it within a few seconds and
   requests a certificate. The status is *awaiting confirmation*, and nothing else changes:
   `http://<server-ip>:8080` keeps working exactly as before.
2. **Open the link the page shows** — `https://<domain>/…/panel-domain` — and sign in there.
   Sessions are per address, so this is a second sign-in, this time over HTTPS.
3. **Confirm.** Krill accepts the confirmation only from a request that actually came through
   the domain over HTTPS, and only once the gateway serves a trusted certificate for it (with
   `KRILL_ACME_STAGING=true` the certificate check is skipped). If the certificate is not
   issued yet, wait a minute and press it again. A check from the server itself would prove
   nothing: the host reaches its own address through loopback even when the outside world
   cannot.

From then on:

- Plain `http://<domain>` redirects to HTTPS, and cookies set on the domain carry `Secure`
  (cookies set on `:8080` don't; both are covered by a regression test).
  **Leave `KRILL_COOKIE_SECURE` off** — requests on `:8080` still get ordinary cookies, so
  signing in there keeps working as a way back.
- Behind the gateway every request comes from Traefik's address. The login rate limiter takes
  the client address Traefik recorded instead — but only for requests that carry a token only
  the gateway adds. A request sent straight to `:8080` can't claim a different address.
  `KRILL_TRUST_PROXY` isn't needed for this.
- Webhook URLs shown on apps use `https://<domain>`, unless `KRILL_PUBLIC_URL` says otherwise.

**Allowed IPs** limit the UI on the domain to your addresses, one IP or CIDR per line.
Webhooks, the REST API and MCP (`/webhooks/`, `/api/`, `/mcp`) stay reachable from anywhere:
GitHub, CI and agents don't come from your addresses, and those endpoints check their own
secrets. Krill refuses to save a list that leaves out the address you are working from. There
is no basic-auth option for the panel. It would take over the `Authorization` header, which
the API and MCP need for their bearer tokens, and the panel already has its own sign-in with a
rate limit.

### HSTS

Without HSTS, a browser that has never seen the panel sends its first request to
`http://<domain>` in clear text and only then gets redirected. **HSTS** (on the same page, once
the domain is confirmed) tells browsers to go straight to HTTPS for that domain. They remember
this for the period you pick, from 5 minutes to 1 year.

- **Off by default.** While off, responses carry `Strict-Transport-Security: max-age=0`, which
  makes a browser forget a policy an earlier setting taught it on its next visit.
- **Start short.** A browser that learned the policy refuses plain HTTP on the domain until it
  expires, and you can't reach a browser that has stopped visiting. Raise the period once the
  domain has proven stable.
- **Only this domain.** The header never carries `includeSubDomains` or `preload`, so hosts
  next to the panel, such as other apps under the same parent domain, are unaffected.
- It is sent only on responses that went through the gateway over HTTPS for the panel's own
  host, never on `http://<server-ip>:8080`. It can't be turned on before the domain is
  confirmed, and changing or removing the domain resets it to off.

### Close direct access

With the domain confirmed and the [cluster firewall](../architecture.md#nodes)
lockdown on, **Close direct access** stops the control plane from accepting `:8080` from the
internet. Only the gateway can reach it after that: its traffic to the host arrives on
`docker_gwbridge`, and the ruleset still lets that in. Start it from the panel opened
through the domain. It works like the lockdown: the change reverts on its own within two
minutes unless you press **Keep direct access closed** on the same page, through the domain.
The confirmation arriving is the proof that the gateway can still reach Krill.

While direct access is closed, the domain can't be changed or removed. Reopen direct access
first. **Open cluster** on the Firewall page reopens it too, and a lockdown started afterwards
leaves `:8080` open until you close it again.

### If the panel is unreachable

- **Direct access still open:** sign in at `http://<server-ip>:8080` and change or remove the
  domain.
- **Direct access closed, the domain is broken** (DNS moved, certificate failing): over SSH,
  remove the lockdown and turn the domain off, then sign in on `:8080`:

  ```sh
  nft delete table inet krill
  docker exec krill-postgres psql -U krill -c "UPDATE panel_gateway SET host='', state='off', direct_port_closed=false, direct_port_close_pending=false"
  ```

  The gateway drops the domain's route on its next poll, within five seconds. (With an
  external state database, run the same `UPDATE` there.)
- **Nothing answers on the domain at all:** check `docker service logs krill-traefik`. A
  message like `Cannot fetch configuration data` means the gateway can't reach Krill at
  `KRILL_ADVERTISE_ADDR:8080`.

## Raw TCP and UDP ports

For services that don't speak HTTP — an SSH port for Gitea, mail, DNS, a game server — publish
a host port straight into the container on the app's **Advanced** tab. These ports bypass
Traefik and the organization network isolation, and they apply on the next deploy.
