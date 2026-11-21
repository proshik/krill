# Installing Krill

Krill installs on a Linux server as a host binary managed by systemd, next to its
own Postgres and a Docker Swarm. One command does all of it.

- [Requirements](#requirements)
- [Install](#install)
- [Installer options](#installer-options)
- [Use a managed / external Postgres](#use-a-managed--external-postgres)
- [After the install](#after-the-install)
- [Observability](#observability)
- [Upgrade](#upgrade)
- [Uninstall](#uninstall)
- [Run the container image instead](#run-the-container-image-instead)

## Requirements

- A Linux server, **amd64 or arm64**, with root access. Krill is built to fit a box as
  small as 2 vCPU / 2 GB RAM.
- Ports **80** and **443** free for app traffic and the UI's domain (Traefik), and **8080**
  for the Krill UI until it has a domain.
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
| `KRILL_VERSION=v0.1.1` | Install a specific release instead of the latest. |
| `KRILL_DOMAIN=apps.example.com` | Base domain for apps (`KRILL_BASE_DOMAIN`). |
| `KRILL_ACME_EMAIL=you@example.com` | Let's Encrypt contact. |
| `KRILL_ADVERTISE_ADDR=203.0.113.10` | Swarm advertise address. Auto-detected by default, and written to `krill.env` either way — worker nodes join at it. |
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
2. Point an `A` record at the server and set the base domain for apps (`KRILL_BASE_DOMAIN`
   in `/etc/krill/krill.env`).
3. Serve the Krill UI on its own domain over HTTPS: **Settings → Panel domain**. Until you
   do, the admin password crosses the network in clear text on every sign-in. The steps, the
   confirmation and how to back out are in
   [Serve the Krill UI on a domain](guides/domains.md#serve-the-krill-ui-on-a-domain).

Leave `KRILL_COOKIE_SECURE=false`. Requests that reach the UI through its domain over HTTPS
get secure cookies without it, and setting it to `true` breaks sign-in at
`http://<server-ip>:8080` — your way back in if the domain ever fails. It is only for a TLS
proxy of your own in front of Krill.

Every setting is listed in [Configuration](configuration.md). Logs:
`journalctl -u krill -f`.

## Observability

Krill can run a Grafana Alloy agent on every node to ship host metrics and container logs to
your own Prometheus-compatible storage and Loki — nothing is kept by Krill itself. It's opt-in
from **Settings → Observability** and needs no installer option or environment variable. See
[Observability](guides/observability.md) for requirements, setup and what it costs on a small
server.

## Upgrade

### From the UI

**Settings → Updates**, instance admins only. Krill checks GitHub for a newer release once a
day (`KRILL_UPDATE_CHECK_INTERVAL`; `0` disables the background check, **Check now** always
works) and puts an "update" badge on the Settings link in the sidebar when one is found.

Clicking **Update** downloads that release's binary and `checksums.txt`, verifies the SHA-256
— the same trust model as `install.sh`: TLS to GitHub plus the checksum file from the same
release, no separate signature — smoke-tests the download with `--version`, keeps the current
binary as `krill.prev`, replaces it, and restarts Krill. While it restarts, the UI answers a
"Krill is starting" page for a few seconds; the Updates page reloads itself and then shows
"Updated from X to Y".

The button is disabled, with the reason shown, unless Krill is running the way `install.sh`
installs it — Linux, systemd, root, with the binary's directory, `/etc/systemd/system` and
`/run` all writable — and it is offered only to a release build (a binary built from source
reports a development version and is never offered an update). It also refuses while a deploy,
a database or volume backup/restore, or a database instance's node move is running. That check
runs when you click and again once the download is verified, just before anything is replaced
and Krill restarts; work started in the last seconds before the restart can still be
interrupted. The container image never self-updates — pull a new tag instead (see
[Run the container image instead](#run-the-container-image-instead)).

If the new binary does not come up within 10 minutes, it is rolled back automatically — see
[Rollback](#rollback) below.

### With the installer

```sh
curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
```

It downloads the latest release binary and restarts the service; Postgres, its data and the
generated secrets are kept. Database migrations run automatically when the new binary starts.

Needed when a release changes something the running binary can't change about itself — the
systemd unit, the bundled Postgres container, `krill.env` — its release notes then say
"requires re-running install.sh". Otherwise it is just another valid way to upgrade. It keeps
the update drop-in from a prior UI update but removes `krill.prev`: that binary may be several
releases old by then, so the **Roll back** button disappears until the next update from the UI.

### Rollback

**Automatic:** a UI update arms a 10-minute timer before it restarts. If the new binary has
not taken over serving the UI by the time the timer fires, the timer puts `krill.prev` back
and restarts Krill; the Updates page then reports the update as rolled back. Rebooting inside
that window clears the timer — the new binary then starts unguarded, and `krill.prev` stays in
place either way. Stopping Krill inside the window does not cancel the timer: when it fires, it
puts the previous binary back and starts Krill again. To keep Krill stopped, first run
`systemctl stop krill-update-revert.timer`. A unit hardened with `ProtectSystem=strict`
(`install.sh` does not set this) needs `ReadWritePaths=/usr/local/bin /etc/systemd/system /run`
added, or the Updates page reports the binary's directory as read-only and never offers an
update.

**Manual:** the Updates page offers **Roll back to vP** when `krill.prev` is a release older
than the running one, no timer involved. By SSH, the same swap:

```sh
mv /usr/local/bin/krill.prev /usr/local/bin/krill && systemctl reset-failed krill && systemctl restart krill
```

`krill --version` prints the version of whichever binary is installed and needs no
environment at all, so it is safe to run first.

Neither kind of rollback undoes database changes made by the newer release — only the binary
goes back.

Rolling back to a release without the Valkey engine (v0.6.0 and earlier) keeps Valkey
instances listed, but that binary has no driver for them: do not deploy, start, stop, change
or delete a Valkey instance from it — on those releases the action crashes Krill (it restarts on
its own). Apps keep the connection values injected by their last deploy. Releases from this one
on refuse such an action with an error instead.

The same releases do not know bucket links (or buckets, systems and bucket backups, which they
simply do not show). A container keeps the bucket values it was started with, but redeploying a
bucket-linked app from such a release starts it **without** them. Do not redeploy those apps
until you upgrade again, or put the values into the app's environment by hand first (read them
on the bucket page; the secret key is shown to admins).

Rolling back to a release without the [observability agent](guides/observability.md) leaves
the agent running with nothing managing it; remove it with `docker service rm krill-alloy-node`
and its configs and secrets labelled `krill.observability=node-agent` (see
[Turning it off](guides/observability.md#7-turning-it-off--troubleshooting)).

**A failed migration is not rolled back.** Migrations run when the new binary starts. If one
fails, or Krill is killed in the middle of one, the database is left marked "dirty" and no
binary starts against it — the previous one the timer puts back included. `journalctl -u krill`
then shows `database schema is dirty at version N`. (`systemctl stop` and the timer's restart
let a running migration finish, as long as it does within systemd's stop timeout — 90 seconds
by default; a `kill -9` or a crash does not.) Recovery is manual and careful:

1. Back up Krill's database first — for the bundled Postgres,
   `docker exec krill-postgres pg_dump -U krill krill > krill-backup.sql`. If
   [Krill backups](guides/krill-backups.md) are on, the newest snapshot under `pre-update/`
   was taken just before the update and is the safer starting point (see
   [Restore Krill's state from a backup](#restore-krills-state-from-a-backup), runbook B).
2. Read in `journalctl -u krill` which migration failed and why; its SQL is
   `internal/database/migrations/<N>_*.up.sql` in that release's source.
3. Fix the data or the schema by hand until it matches version N either fully applied or not
   applied at all.
4. Mark the version clean (`docker exec -it krill-postgres psql -U krill krill`): after
   verifying the schema matches version N, `UPDATE schema_migrations SET dirty = false;` — or,
   to have migration N run again, `UPDATE schema_migrations SET version = <N-1>, dirty = false;`.
5. `systemctl restart krill`.

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

## Restore Krill's state from a backup

This applies when [Krill backups](guides/krill-backups.md) were set up. The backup restores
Krill's **configuration**, not your workloads. After a restore:

- Swarm services must be redeployed.
- App volumes, app databases and buckets come back only from their own backups.
- Worker nodes belonged to the old swarm and must be removed and re-added **before** you redeploy.

You need the age private key (`krill-backup-key.txt`) and, to read the encrypted secrets, the old
`KRILL_SECRET_KEY`. Add a lifecycle rule that aborts incomplete multipart uploads after one day
to the backup bucket; a Krill killed mid-backup would otherwise leave billed, invisible leftovers.

### Restoring the dump

Both runbooks use this procedure. Use `bash` (the `pipefail` option below is not in every `sh`).
Without it a wrong key or a damaged object can look like a successful restore: `psql` sees an
empty input, or only the beginning of the dump, and commits that.

1. **Verify the object first**, before touching any database. This decrypts and decompresses the
   whole object and throws the result away; every stage must succeed:
   ```sh
   set -o pipefail
   age -d -i krill-backup-key.txt <object> | gunzip > /dev/null && echo "object OK"
   ```
   Stop here if it does not print `object OK`.
2. Recreate the target database empty. For the bundled Postgres:
   ```sh
   docker exec krill-postgres psql -U krill -d postgres -c 'DROP DATABASE krill' -c 'CREATE DATABASE krill OWNER krill'
   ```
3. Restore from the same object, still with `pipefail` set:
   ```sh
   age -d -i krill-backup-key.txt <object> | gunzip | docker exec -i krill-postgres psql -v ON_ERROR_STOP=1 --single-transaction -U krill -d krill \
     && echo "restore OK"
   ```
   If this does not print `restore OK`, do not start Krill and do not retry on top of it: `psql`
   may already have committed part of the dump. Repeat step 2, then step 3.
4. **Check the result before starting Krill.** The schema must be clean and the data present:
   ```sh
   docker exec krill-postgres psql -U krill -d krill -c 'SELECT version, dirty FROM schema_migrations' -c 'SELECT count(*) FROM applications'
   ```
   `dirty` must be `f`, and the counts must look like your installation.

**Match the client to the dump.** Recent `pg_dump` releases write `\restrict` / `\unrestrict`
lines, and a `psql` that is too old for them rejects the dump. Check for the line and read the
header (the "Dumped by pg_dump version" line sits a few lines down, after `\restrict`):

```sh
age -d -i krill-backup-key.txt <object> | gunzip | grep -m1 '^\\restrict'
age -d -i krill-backup-key.txt <object> | gunzip | sed -n '1,12p'
psql --version
```

If the dump contains `\restrict`, use a `psql` of at least 13.22, 14.19, 15.14, 16.10 or 17.6
within its own major, or any 18 or newer: a `psql` of a different major below its own threshold
(for example 17.5) rejects the dump even when its version number is larger. The simplest safe
choice is the `postgres:<dump major>-alpine` image (the same image Krill dumps with).

Check this before you reset anything: a restore that stops on the first line of the dump has
already lost the old data. If your `psql` is older, prefix every `psql` below with
`docker run --rm -i --network host -e PGHOST -e PGPORT -e PGUSER -e PGSSLMODE -e PGPASSFILE -v "$PGPASSFILE:$PGPASSFILE:ro" postgres:<major>-alpine`
(drop `--network host` and the SSL variables you do not need; a `verify-full` connection also
needs the CA file mounted and `PGSSLROOTCERT` set).

For an **external database**, do not put the connection string on the command line: it would be
visible to every process listing and audit log on the host. Use libpq's environment variables and
a password file readable only by you instead, and run the same `psql` command without
`docker exec`. First read the real database name, user and host from `KRILL_DATABASE_URL` in
`/etc/krill/krill.env` (the path is its database; `<db>` and `<owner>` below mean those values,
not necessarily `krill`) and use them in every command, including the checks:

```bash
export PGHOST=db.example.com PGPORT=5432 PGUSER=<owner> PGSSLMODE=verify-full
export PGPASSFILE="$HOME/.krill-pgpass"
# Type the password at the silent prompt (in bash); it never appears on a command line or in shell history.
# libpq's password file needs \\ for a backslash and \: for a colon, so escape them first.
( umask 077; read -rs -p 'Database password: ' PW && echo \
  && PW=$(printf '%s' "$PW" | sed 's/[\\:]/\\&/g') \
  && printf '%s:%s:*:%s:%s\n' "$PGHOST" "$PGPORT" "$PGUSER" "$PW" > "$PGPASSFILE" ); unset PW
```

(Or create the file with an editor under `umask 077`. Do not use a literal password in a command.)
Then run steps 2 to 4 with `<db>` in place of `krill` and without `docker exec`; step 2 becomes:

```sh
psql -d postgres -c 'DROP DATABASE "<db>"' -c 'CREATE DATABASE "<db>" OWNER "<owner>"'
```

A managed service may not let you drop the database, or your role may only own objects inside
it. Then reset the schemas instead, and do not assume they are only `public`. List what exists
and what the dump creates:

```sh
psql -d <db> -c '\dn'
age -d -i krill-backup-key.txt <object> | gunzip | grep '^CREATE SCHEMA'
```

Drop every non-system schema that appears in either list with `DROP SCHEMA "<name>" CASCADE`,
then `CREATE SCHEMA public` if it is gone (or use the provider's own reset). The dump has no
`CREATE DATABASE` and does not carry database-level settings, so a `search_path` set with
`ALTER DATABASE` or `ALTER ROLE` is not restored: if your installation had one, set it again
(`ALTER DATABASE "<db>" SET search_path = ...`) before step 3. For the check in step 4, connect
exactly as Krill does (same `KRILL_DATABASE_URL`, so its options apply) and read
`SHOW search_path` together with `SELECT version, dirty FROM schema_migrations`: it must find the
restored table without schema qualification. Finally `rm -f "$PGPASSFILE"`. To keep the private
key off the host, decrypt on your workstation and pipe the result over `ssh`.

### A. Lost host, new host

1. Run `install.sh` (for an external database, with the new `KRILL_DATABASE_URL`). Use a release
   equal to or newer than the snapshot's; `install.sh` installs the latest. Never use an older
   one.
2. `systemctl stop krill`. The first start already migrated and seeded an empty database.
3. In `/etc/krill/krill.env`, set the **original** `KRILL_ADMIN_EMAIL` and `KRILL_SECRET_KEY`.
   Setting `KRILL_ADMIN_EMAIL` when running `install.sh` does not carry over: the installer
   always writes `admin@krill.local` (or `admin@<KRILL_DOMAIN>`). Krill promotes the account named
   there to instance operator on every start and demotes every other one, so with a different
   address your restored operator loses access to Nodes, Firewall and the other pages the rest of
   this runbook needs. Without the old `KRILL_SECRET_KEY` the encrypted secrets are unreadable and
   the operations that need them fail visibly.
4. Fetch the backup object with your storage's own tool, onto your workstation or the host.
5. Restore the dump as described in [Restoring the dump](#restoring-the-dump), including the
   checks, **before** the next step.
6. `systemctl start krill`. The admin password is the old one (the seeding step never overwrites
   an existing user). Migrations bring an older snapshot forward.
7. **Bring the cluster back before redeploying anything**, because the restored state refers to
   the old swarm:
   - The restored `cluster_nodes` rows describe workers of the old swarm. The Nodes page lists
     only nodes the **live** swarm reports, so a restored worker that has not joined the new
     swarm has no row there and no Remove button, yet its name still blocks adding a worker
     with the same name. Clean them up by hand, with Krill stopped (`systemctl stop krill`),
     before you rejoin the workers:
     ```sh
     docker exec -i krill-postgres psql -U krill -d krill -c 'SELECT id, name, swarm_node_id FROM cluster_nodes'
     docker exec -i krill-postgres psql -U krill -d krill -c "DELETE FROM node_labels WHERE swarm_node_id IN (SELECT swarm_node_id FROM cluster_nodes WHERE swarm_node_id <> '')" \
       -c "DELETE FROM firewall_checks WHERE node LIKE 'node-%'" -c 'DELETE FROM cluster_nodes'
     ```
     (With an external database, use your `psql` setup and the real database name instead of
     `docker exec`.) Nothing has a foreign key to `cluster_nodes`; `node_labels` and
     `firewall_checks` only hold display names and last-check times for the old IDs. Start
     Krill again, and on the Nodes page add each worker again by SSH (a worker still in another
     swarm must leave it first: `docker swarm leave --force`). Every worker comes back with a
     **new** Swarm node ID. A worker whose firewall Krill had locked down keeps its persisted
     ruleset, which allows only the OLD cluster's addresses and can block the new swarm.
     Before re-adding such a worker, run on it, as root, what Krill's own node removal does:
     ```sh
     systemctl stop krill-fw-revert.timer 2>/dev/null; nft delete table inet krill 2>/dev/null; rm -f /etc/krill/firewall.nft; systemctl disable krill-firewall.service
     ```
     Re-apply a lockdown afterwards from the Firewall page if you want one.
   - Apps pinned to nodes (placement `pin` or `global`) store the old node IDs, and a deploy
     rejects an ID that no longer exists. Open each such app's Advanced tab, choose its nodes
     again from the list and save (or switch it to "Any node").
   - A database instance stores the **hostname** of the node it was pinned to. If that node is
     back under the same name, nothing to do. If it is gone, the instance cannot start and the
     usual "change node" action cannot help, because it tries to copy the volume from the old node.
     The data on a lost node is not recoverable from this backup anyway, so point the instance at
     a node that exists (empty means the control-plane node), and it will start empty:
     `UPDATE db_instances SET node_hostname = '<hostname>' WHERE id = <id>;` in Krill's database
     (`docker exec -i krill-postgres psql -U krill -d krill`).
8. Recover the workloads:
   - Do not leave the restored backup schedules on: against an empty database, volume or bucket
     a scheduled run uploads an empty backup and, through its retention count, deletes the last
     populated one you need. Pause them **in step 5, right after the restore checks and before
     `systemctl start krill`** (the scheduler reads these flags at start), in the same `psql`
     session as the restore:
     ```sql
     UPDATE backups SET enabled = false;
     UPDATE volume_backups SET enabled = false;
     UPDATE bucket_backups SET enabled = false;
     ```
     Resume them from the Backups pages only after every restore below has finished and been
     checked. Alternatively copy the objects you will restore from to a prefix no schedule
     writes to.
   - Point DNS at the new IP address.
   - Redeploy the database instances.
   - For each logical database, recreate its user and database by hand: a redeployed instance
     starts empty and Krill does not re-create them. Take the name, user and password from the
     database's page and run this in the instance's container as its superuser
     (`docker exec -i <instance container> psql -U <superuser>`):
     ```sql
     CREATE USER "<user>" PASSWORD '<password>';
     CREATE DATABASE "<db>" OWNER "<user>";
     REVOKE CONNECT ON DATABASE "<db>" FROM PUBLIC;
     ```
   - Restore the app database backups, then the volume and bucket backups. A bucket is restored
     into a **new** bucket, and each restore must finish first (see
     [Buckets](guides/buckets.md#back-up-and-restore-a-bucket)). Then relink each app to the
     restored bucket, and change any bucket name, endpoint or key written literally in the app's
     environment, before you redeploy: an app redeployed with its old bucket links points at a
     bucket that is gone or empty.
   - Redeploy the apps.
   - Resume the backup schedules you paused (the Pause/Resume toggle on each backup).
   - Re-check Settings → Panel domain and the Firewall page.

### B. Failed upgrade on the same host

Use this for a dirty migration or broken data after an update.

1. **Cancel the update's rollback machinery first.** For ten minutes after an update, a timer
   restarts Krill (and may swap the binary) if the new one has not confirmed its start; a
   restart in the middle of a restore could hold a connection that blocks the drop, or run
   migrations on the empty database before the import finishes:
   ```sh
   systemctl stop krill-update-revert.timer krill-update-restart.timer 2>/dev/null
   systemctl stop krill-update-revert.service krill-update-restart.service 2>/dev/null
   rm -f /run/krill-update-pending
   systemctl list-timers 'krill-update-*'    # must list nothing
   ```
2. `systemctl stop krill`.
3. Put the old binary back (`mv /usr/local/bin/krill.prev /usr/local/bin/krill`, or download the
   release you updated from).
4. Restore the **pre-update** snapshot whose name ends in `_<from>_to_<to>.sql.gz.age`, as in
   [Restoring the dump](#restoring-the-dump), including the checks.
5. `systemctl start krill`. `KRILL_SECRET_KEY` has not changed.

The workloads kept running, so nothing needs redeploying except what the failed release touched.

### Test a restore regularly

Restore into a throwaway container, check it, and remove it. Use the image of your server's own
Postgres major version (`postgres:<major>-alpine`; the bundled install runs 17):

```sh
docker run -d --rm --name krill-restore-test -e POSTGRES_PASSWORD=x postgres:<major>-alpine
# verify and pipe the snapshot into it as in "Restoring the dump", with `docker exec -i krill-restore-test psql -U postgres`
docker exec krill-restore-test psql -U postgres -c 'SELECT version, dirty FROM schema_migrations' -c 'SELECT count(*) FROM applications'
docker rm -f krill-restore-test
```

This proves both the object and the private key.

## Uninstall

```sh
systemctl stop krill-update-revert.timer krill-update-restart.timer 2>/dev/null   # first: a timer could restart krill
systemctl disable --now krill
rm -f /etc/systemd/system/krill.service /usr/local/bin/krill /usr/local/bin/krill.prev
rm -f /etc/systemd/system/krill.service.d/10-krill-update.conf
rmdir /etc/systemd/system/krill.service.d 2>/dev/null
systemctl daemon-reload
rm -rf /etc/krill
docker rm -f krill-postgres && docker volume rm krill-pg-data   # destroys Krill's state
docker network rm krill-state                                   # after the container is gone
```

This leaves behind what Krill created in Docker: the services for your apps, database
instances and Traefik (`krill-traefik`), their volumes and the overlay networks, and — if the
observability agent was ever turned on — the `krill-alloy-node` service, its configs and
secrets (labelled `krill.observability=node-agent`) and the `krill-alloy-node-data` volume on
every node. Delete apps and databases from the UI and turn observability off before
uninstalling, or remove the rest with `docker service rm`, `docker volume rm` and
`docker network rm` afterwards:

```sh
docker service rm krill-alloy-node
docker config rm $(docker config ls -q --filter label=krill.observability=node-agent)
docker secret rm $(docker secret ls -q --filter label=krill.observability=node-agent)
docker volume rm krill-alloy-node-data   # on every node, once the agent's task is gone
```

## Run the container image instead

The control plane is also published as a multi-arch image, `ghcr.io/proshik/krill`, tagged
with the release version without the leading `v` (`0.1.1`), the minor version (`0.1`) and
`latest`. It is a static binary on a distroless base, about 25 MB, and needs
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
