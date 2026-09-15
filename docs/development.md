# Development

- [Prerequisites](#prerequisites)
- [Run locally](#run-locally)
- [Generated code](#generated-code)
- [Tests](#tests)
- [Make targets](#make-targets)
- [CI and releases](#ci-and-releases)

Contribution rules — conventions, what a pull request needs — are in
[CONTRIBUTING.md](../CONTRIBUTING.md).

## Prerequisites

- **Go 1.26.1**
- **Docker** with Swarm initialized: `docker swarm init`. On macOS,
  [Colima](https://github.com/abiosoft/colima) works in place of Docker Desktop.
- **Tailwind CSS v4 standalone binary** in `./tools/tailwindcss` — no Node.js or npm:

  ```sh
  ./tools/get-tailwind.sh
  ```

  The script pins Tailwind v4.3.0 for macOS and Linux (arm64 / x86_64). `make run` and
  `make build` regenerate CSS with it, so download it first.

`templ` and `sqlc` need no installation — they run through `go tool`.

## Run locally

```sh
docker swarm init                   # once, if not initialized
make db-up                          # Postgres 16 on :5432 (user, password, db: krill)
cp .env.example .env                # then adjust KRILL_DOCKER_HOST for your Docker
set -a && . ./.env && set +a        # the binary reads the environment, not the file
make run                            # regenerates code, then starts on :8080
```

Open <http://localhost:8080> and log in as `admin@krill.local` / `changeme` (from
`KRILL_ADMIN_EMAIL` / `KRILL_ADMIN_PASSWORD`). If `8080` or `5432` are taken, change
`KRILL_LISTEN_ADDR` and the port in `KRILL_DATABASE_URL`.

On startup Krill runs migrations, creates the admin user and a `Default` organization, starts
Traefik on host port 80, and begins listening. Apps get `*.127-0-0-1.sslip.io` domains, which
resolve to localhost with no DNS setup.

All settings are described in [Configuration](configuration.md).

### Migrations

A migration must keep the *previous* release's binary able to start against the schema it
leaves behind: only additive changes are allowed — new tables, new columns that are nullable
or carry a `DEFAULT`, new non-unique indexes. New unique indexes and foreign keys on existing
tables are not additive, an index the previous release relies on for `ON CONFLICT` must never be
dropped, and the previous binary must tolerate new enum or `CHECK` values the new one writes —
widen reads a release before writes. Dropping or renaming something, changing a column's type,
adding `NOT NULL` (as a new column or via `SET NOT NULL`) with no default, dropping `NOT NULL`,
or adding a `CHECK` that could reject what the previous release writes is safe only one release
after the code has stopped using the old shape (expand/contract). Dropping a constraint is always
fine. This is what makes automatic rollback on a failed self-update possible:
`database.RunMigrations` (`internal/database/migrate.go`) skips applying migrations (with a
warning log) when the database schema is already ahead of what the running binary embeds,
instead of refusing to start.

CI checks every migration above version 46 with [squawk](https://squawkhq.com), a Postgres
migration linter. To run the same check locally:

```sh
./tools/get-squawk.sh    # once: downloads the pinned, checksum-verified binary to tools/squawk
make lint-migrations
```

`.squawk.toml` keeps only the rules about compatibility with the previous binary (dropping or
renaming a column or table, changing a column type, `SET NOT NULL`, a `NOT NULL` column without a
default, `ADD CONSTRAINT ... UNIQUE`, a new foreign key, `DROP NOT NULL`) and turns the rest off.
The target also checks the rule set against `internal/database/testdata/squawk/breaking.sql`
(must fail, reporting every rule it expects) and `compatible.sql` (must pass). A unique index or
foreign key added to a table created earlier in the same migration is not flagged.

squawk checks the mechanical cases; reviewers still read every migration. It does not catch a new
or tightened `CHECK` constraint, enum widening the previous binary cannot handle, dropping an
index used by `ON CONFLICT`, or `CREATE UNIQUE INDEX` on an existing table.

A statement that intentionally breaks the policy needs `-- squawk-ignore <rule>` on the line
directly before it, followed on the same line by a comment with the reason and the release it is
safe from:

```sql
-- squawk-ignore ban-drop-column -- safe from v0.5.0: nothing since v0.4.0 reads legacy_note
ALTER TABLE users DROP COLUMN legacy_note;
```

The ignore applies to the line squawk reports, so keep nothing between it and the statement (a
reason on its own line goes above the ignore), and in a multi-line statement put it right above
the offending clause.

## Generated code

Templ components (`*_templ.go`), sqlc queries (`internal/database/gen`) and the compiled CSS
(`internal/web/static/app.css`) are **committed**. After editing any `.templ`, `.sql` or
`input.css` file, run:

```sh
make generate
```

CI fails when the committed output is out of date. sqlc does not delete output for a query
file you removed — delete the matching `internal/database/gen/*.sql.go` by hand.

## Tests

```sh
make test               # unit and database tests
make test-integration   # adds tests tagged `integration`
```

Database tests start throwaway Postgres (and MinIO) containers through testcontainers.

**Colima:** the testcontainers cleanup container can't mount Colima's socket, and tests fail
with a socket-mount error unless both of these are set. `make test` sets them:

```sh
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
```

With native Docker on Linux, run `go test ./...` directly instead.

**End to end:** [`scripts/e2e.sh`](../scripts/e2e.sh) starts an isolated Postgres (`:55432`),
Krill (`:18080`) and Traefik (host `:80`), then walks through login, roles, image and
Dockerfile deploys, rolling updates, routing and a managed Postgres. It cleans up after itself
and exits non-zero on any failed check.

### Self-update acceptance

[`test/acceptance/`](../test/acceptance/) proves **Settings → Updates** on a real systemd host:
it builds five throwaway releases of the current `HEAD` (`v0.90.0`–`v0.90.4`, with extra
migrations, a crash right after migrating, a 45-second migration and a failing one), installs
`v0.90.0` with `install.sh` into a disposable [Lima](https://lima-vm.io) VM (Ubuntu 24.04,
4 CPUs, 6 GiB), and drives the Updates page over a port forward to `127.0.0.1:28080`. The
tests carry the `acceptance` build tag and do nothing unless `KRILL_ACCEPT=1`, so `make test`
never runs them.

Prerequisites: `limactl` 2.x, Go, and for the full run the `gh` CLI signed in with write access
to the test repository (default `proshik/krill-update-e2e`, public, with at least one commit).
It runs in two phases:

```sh
make acceptance-vm-smoke     # builds the releases, creates the VM, installs v0.90.0, checks the page
make acceptance-selfupdate   # the full scenario chain
```

- **VM smoke** writes nothing to GitHub. The first run downloads the Ubuntu image and installs
  Docker in the VM: 10–25 minutes.
- **The full run publishes the five releases to the test repository** and moves its "latest"
  release with `gh release edit --latest` as the scenarios go: a deploy refusing the update,
  an update and a manual rollback, a crash-looping release put back by the 10-minute timer,
  `install.sh` over a UI update with `systemctl stop` in the middle of a migration, and the
  documented recovery from a failed migration. The two timer scenarios wait for the timer, so
  expect well over an hour.

Settings, all optional: `KRILL_ACCEPT_VM` (`krill-accept`), `KRILL_ACCEPT_REPO`,
`KRILL_ACCEPT_HOST_PORT` (`28080`), `KRILL_ACCEPT_ARCH` (`arm64`), `KRILL_ACCEPT_WORKDIR`
(`.superpowers/acceptance`, git-ignored: exported sources, built releases, the Lima template
and `report.md` with the evidence of every step). The VM is deleted at the end unless
`KRILL_ACCEPT_KEEP_VM=1`; a failed step stops the chain and keeps the VM for inspection
(`limactl shell krill-accept`). Each run starts from a fresh install inside the VM, so a kept
VM can be reused. Delete it with `limactl delete --force krill-accept`.

## Make targets

| Target | What it does |
|--------|--------------|
| `make generate` | templ, sqlc and minified CSS. |
| `make run` | `generate`, then `go run ./cmd/krill`. |
| `make build` | `generate`, then build `bin/krill`. |
| `make build-cli` | Build `bin/krill-cli` (needs no generated code). |
| `make test` | All tests, with the Colima socket settings. |
| `make test-integration` | Tests tagged `integration`. |
| `make acceptance-vm-smoke` | Self-update acceptance, VM part only (see above). |
| `make acceptance-selfupdate` | Full self-update acceptance; publishes test releases. |
| `make css` / `make css-watch` | Rebuild CSS once / on change. |
| `make db-up` / `make db-down` | Start / stop the development Postgres. |
| `make tidy` | `go mod tidy`. |

`make build` stamps the output of `git describe --tags --always --dirty` into
`internal/buildinfo.Version`, so a binary built from a checkout — anything short of the
release workflow's own build — reports a development version, and **Settings → Updates**
never offers it an update. To exercise that page itself, point `KRILL_UPDATE_REPO` at a fork
and publish a real tagged release there.

## CI and releases

- [`ci.yml`](../.github/workflows/ci.yml) runs on every push and pull request: `go build`,
  `go vet`, `go test`, a Docker image build without pushing, and a check that generated code
  is up to date.
- [`release.yml`](../.github/workflows/release.yml) runs on a `v*.*.*` tag. It pushes a
  multi-arch image to `ghcr.io/proshik/krill` (tags without the leading `v`, plus `latest`) and
  creates a GitHub Release with static `krill-linux-amd64` / `krill-linux-arm64` binaries,
  `krill-cli` tarballs for Linux and macOS, and `checksums.txt` — the files `install.sh`
  downloads and verifies.

```sh
git tag v0.2.0 && git push origin v0.2.0
```

To check the image locally: `docker build -t krill:local .` The build uses the committed
generated files and does not run `make generate`.
