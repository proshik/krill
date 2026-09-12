# Installing Krill

Krill installs on a Linux server as a host binary managed by systemd, next to its
own Postgres and a Docker Swarm. One command does all of it.

- [Requirements](#requirements)
- [Install](#install)
- [Installer options](#installer-options)
- [Use a managed / external Postgres](#use-a-managed--external-postgres)
- [After the install](#after-the-install)
- [Upgrade](#upgrade)
- [Uninstall](#uninstall)
- [Run the container image instead](#run-the-container-image-instead)

## Requirements

- A Linux server, **amd64 or arm64**, with root access. Krill is built to fit a box as
  small as 2 vCPU / 2 GB RAM.
- Ports **80** and **443** free for app traffic (Traefik), and **8080** for the Krill UI.
- Docker is installed for you if it is missing.

## Install

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
```

The installer:

1. installs Docker (via `get.docker.com`) if missing and initializes a single-node Swarm;
2. writes `/etc/krill/krill.env` with a generated admin password and encryption key
   (`KRILL_SECRET_KEY`), created once and kept on every re-run;
3. runs a loopback-only `postgres:17-alpine` container for Krill's own state, on its own
   `krill-state` Docker network rather than the default bridge — build containers also sit
   on that default bridge and could reach any container on it by IP, which a loopback-only
   port publish does nothing to stop;
4. downloads the `krill` binary from the latest GitHub Release and verifies its SHA-256
   against the release's `checksums.txt` (it refuses to continue without one);
5. installs and starts the `krill` systemd service;
6. prints the admin URL and a one-time login.

## Installer options

Prefix the command with any of these:

| Variable | Effect |
|----------|--------|
| `KRILL_VERSION=v0.1.0` | Install a specific release instead of the latest. |
| `KRILL_DOMAIN=apps.example.com` | Base domain for apps (`KRILL_BASE_DOMAIN`). |
| `KRILL_ACME_EMAIL=you@example.com` | Let's Encrypt contact. |
| `KRILL_ADVERTISE_ADDR=203.0.113.10` | Swarm advertise address (auto-detected by default). |
| `KRILL_DATABASE_URL=postgres://…` | Keep Krill's state in an external Postgres — see below. |
| `KRILL_BINARY=/path/to/krill` | Install a binary already on the host and skip the download — for an air-gapped host or a custom build (`scp` it up first). |
| `KRILL_SKIP_VERIFY=1` | Install a downloaded binary even when the release has no `checksums.txt`. By default the installer fails closed. |

Variables go **after** the pipe, and `sudo -E` passes them through:

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh \
  | KRILL_DOMAIN=apps.example.com sudo -E sh
```

## Use a managed / external Postgres

By default Krill's state lives in the local `krill-postgres` container. To keep it off the
box — managed durability and backups — pass `KRILL_DATABASE_URL`, and the local container is
skipped:

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh \
  | KRILL_DATABASE_URL='postgres://user:pass@db.example.com:5432/krill?sslmode=require' sudo -E sh
```

- Managed providers (Neon, Supabase, RDS, …) require TLS — include `sslmode=require` (or
  `verify-full`) in the DSN.
- Point it at an **empty** database created at the provider; Krill applies its schema on
  first start.
- The installer checks connectivity before writing any configuration and aborts on failure.
  The DSN password is never printed.
- Backups of the state database are then the provider's responsibility.
- The choice is made at **first install**. Re-running the installer on a local install with
  `KRILL_DATABASE_URL` set does **not** move state to the external database; switching
  backends needs a manual dump and restore. An install that already uses an external DSN keeps
  using it on upgrade.

## After the install

1. Open `http://<server-ip>:8080` and log in with the credentials the installer printed.
2. Point an `A` record at the server and set the base domain (`KRILL_BASE_DOMAIN` in
   `/etc/krill/krill.env`).
3. Put the Krill UI behind HTTPS, then set `KRILL_COOKIE_SECURE=true` in
   `/etc/krill/krill.env` and run `systemctl restart krill`.

Every setting is listed in [Configuration](configuration.md). Logs:
`journalctl -u krill -f`.

## Upgrade

Re-run the installer. It downloads the latest release binary and restarts the service;
Postgres, its data and the generated secrets are kept.

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
```

Database migrations run automatically when the new binary starts.

### Upgrading an install from before v0.1.0

An install built before the first release — from source or through `KRILL_BINARY` — gets
several one-time changes on its first start after the upgrade. Read this before upgrading
one:

- **Krill's Postgres container is recreated.** The installer removes `krill-postgres` and
  starts it again on the `krill-state` network — same volume, same data, a brief restart. A
  running container can't be moved to another network in place.
- **Every organization moves onto its own overlay network.** This runs in the background
  once the server is already listening. Moving a service means redeploying it, so a
  Dockerfile app is rebuilt (from the build cache). With several such apps, expect the first
  while after the upgrade to be slower than usual; nothing is down for the whole duration.
  Stopped apps and stopped database instances stay stopped.
  - An organization is recorded as moved only once every deploy of its pass succeeds. If the
    process restarts mid-pass, or one app's redeploy fails, the organization's **whole** pass
    runs again on the next start — including rebuilding its Dockerfile apps — until it
    completes.
- **Default resource limits start applying.** Because the network move redeploys everything,
  it is the first time `KRILL_DEFAULT_MEMORY_LIMIT` / `KRILL_DEFAULT_CPU_LIMIT` (`512m` /
  `1.0`) reach apps and database instances that have no limit of their own. Anything running
  above those values can be OOM-killed or throttled by this background pass. **Before
  upgrading**, set an explicit limit on anything that needs more (the app's Advanced tab, or
  the database instance's settings), or raise or empty the defaults.
- **Private Git hosts are blocked by the egress guard.** A Dockerfile app whose `git_url`
  points at a private address (a Git server on your LAN or on the host itself) now stops
  building on that forced rebuild. Set `KRILL_ALLOW_PRIVATE_EGRESS=true` before upgrading if
  you build from one.

## Uninstall

```sh
systemctl disable --now krill
rm -f /etc/systemd/system/krill.service /usr/local/bin/krill
rm -rf /etc/krill
docker rm -f krill-postgres && docker volume rm krill-pg-data   # destroys Krill's state
docker network rm krill-state                                   # after the container is gone
```

This leaves behind what Krill created in Docker: the services for your apps, database
instances and Traefik (`krill-traefik`), their volumes and the overlay networks. Delete apps
and databases from the UI before uninstalling, or remove the rest with `docker service rm`,
`docker volume rm` and `docker network rm` afterwards.

## Run the container image instead

The control plane is also published as a multi-arch image, `ghcr.io/proshik/krill` (tags
`0.1.0`, `0.1`, `latest`). It is a static binary on a distroless base, about 25 MB, and needs
only a reachable Postgres and a Swarm-enabled Docker daemon:

```sh
docker run -d --name krill -p 8080:8080 \
  -e KRILL_DATABASE_URL='postgres://krill:krill@db-host:5432/krill?sslmode=disable' \
  -e KRILL_ADMIN_EMAIL=admin@example.com \
  -e KRILL_ADMIN_PASSWORD=change-me \
  -e KRILL_SECRET_KEY=<a long random string> \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(stat -c %g /var/run/docker.sock)" \
  ghcr.io/proshik/krill:latest
```

- **Keep `KRILL_SECRET_KEY` the same across restarts.** Stored credentials are encrypted with
  it; a new key makes every one of them unreadable.
- The image runs as a non-root user, so it needs the Docker socket's group (`--group-add`).

This path is not what the installer does and gets less testing: the supported install is the
host binary.
