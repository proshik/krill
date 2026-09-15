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
or carry a `DEFAULT`, new indexes. Dropping or renaming something, changing a column's type,
adding `NOT NULL` (as a new column or via `SET NOT NULL`) with no default, or adding a new
`CHECK` constraint is safe only one release after the code has stopped using the old shape;
dropping a `CHECK` is always fine, and dropping one and re-adding it under the same name with a
wider condition in the same migration counts as safe too (the lint can't tell a widened
re-creation from a tightened one, so review by eye still matters). This is what makes automatic
rollback on a failed self-update possible: `database.RunMigrations`
(`internal/database/migrate.go`) skips applying migrations (with a warning log) when the
database schema is already ahead of what the running binary embeds, instead of refusing to
start. `internal/database/compat.go` lints every migration above version 46 for the disallowed
patterns, one violation per breaking SQL statement; a statement that intentionally breaks this
needs a `-- krill:compat-break-ok <reason>` comment directly above it, explaining which release
it's safe from — the marker exempts only that one statement, not the rest of the file.

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

## Make targets

| Target | What it does |
|--------|--------------|
| `make generate` | templ, sqlc and minified CSS. |
| `make run` | `generate`, then `go run ./cmd/krill`. |
| `make build` | `generate`, then build `bin/krill`. |
| `make build-cli` | Build `bin/krill-cli` (needs no generated code). |
| `make test` | All tests, with the Colima socket settings. |
| `make test-integration` | Tests tagged `integration`. |
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
