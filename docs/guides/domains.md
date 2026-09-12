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

## Raw TCP and UDP ports

For services that don't speak HTTP — an SSH port for Gitea, mail, DNS, a game server — publish
a host port straight into the container on the app's **Advanced** tab. These ports bypass
Traefik and the organization network isolation, and they apply on the next deploy.
