# Development

- [Prerequisites](#prerequisites)
- [Run locally](#run-locally)
- [Generated code](#generated-code)
- [Tests](#tests)
- [Make targets](#make-targets)
- [Landing site](#landing-site)
- [CI and releases](#ci-and-releases)
- [Tasks](#tasks)

Contribution rules — conventions, what a pull request needs — are in
[CONTRIBUTING.md](../CONTRIBUTING.md).

## Prerequisites

- **Go 1.26.11**
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
`testutil.NewTestDB(t)` starts one Postgres container per test binary, migrates a template
database once, and gives each call a fresh database cloned from it, dropped when the test ends.
`testutil.NewMinio(t)` likewise returns one S3 server (the Silo image managed S3 instances run)
per test binary; a test names its buckets with `MinioInfo.Bucket(t, base)` so they never collide.

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
variants' migrations are numbered after the last migration in `HEAD`, so they never collide
with ones the branch gains. The tests carry the `acceptance` build tag and do nothing unless `KRILL_ACCEPT=1`, so `make test`
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

### State-backup acceptance

`TestStateBackupAcceptance` (same package, same prerequisites) runs the state-restore runbooks of
[`docs/install.md`](install.md#restore-krills-state-from-a-backup) with their own commands, on a
disposable Lima VM of its own (`krill-accept-sbk`, port `28081`):

- it installs a throwaway `v0.91.0`, populates it through the UI (an organization, an image app
  from a password-protected registry with a domain and environment variables, a Telegram channel,
  a Postgres server with a logical database and its backup), configures **Settings → Krill
  backups** against a MinIO inside the VM and presses **Back up now**;
- it restores that object into a throwaway container ("Test a restore regularly"), wipes
  everything Krill installed ("Uninstall", the swarm, every volume), reinstalls a newer
  `v0.91.1` and follows runbook A to the recovered workloads;
- it updates to a `v0.91.2` whose migration fails and follows runbook B from the pre-update
  snapshot.

Each restore is checked by a fingerprint of Krill's database (row counts and hashes per table,
the sequences, the schema; `sessions` and `metric_samples` are left out, as the backup leaves
out their data). The commands the test runs are listed in `test/acceptance/accept/runbook.go`,
and a unit test in `make test` fails when `docs/install.md` and that list disagree.

```sh
make acceptance-statebackup
```

Scenario A writes nothing to GitHub; scenario B publishes `v0.91.0`–`v0.91.2` to the same test
repository and moves its "latest" release, so **do not run it at the same time as
`make acceptance-selfupdate`**. It needs no `age` on the Mac (the key is generated in Go; the VM
gets the `age` package). It takes about ten minutes on a kept VM whose image cache is warm. Before
`install.sh` it points the VM's Docker at the `mirror.gcr.io` pull-through mirror and pulls every
image the run needs, restarting dockerd when a pull stalls for five minutes: Docker Hub's CDN has
been seen to stall a download for over an hour. That is the test environment only; the installed
Krill is unchanged. Its
workdir is `.superpowers/acceptance-statebackup` (`report.md` with the evidence and a summary of
every runbook command, `test.log`, the key and the backup object); a last step fails the run if
any secret the harness knows of shows up in either file. `KRILL_ACCEPT_KEEP_VM=1` and the other
settings work as above.

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
| `make acceptance-statebackup` | State-restore runbooks on a VM; scenario B publishes test releases. |
| `make css` / `make css-watch` | Rebuild CSS once / on change. |
| `make db-up` / `make db-down` | Start / stop the development Postgres. |
| `make tidy` | `go mod tidy`. |
| `make site` / `make site-serve` | Build the landing site into `site/public` / preview it locally. |

`make build` stamps the output of `git describe --tags --always --dirty` into
`internal/buildinfo.Version`, so a binary built from a checkout — anything short of the
release workflow's own build — reports a development version, and **Settings → Updates**
never offers it an update. To exercise that page itself, point `KRILL_UPDATE_REPO` at a fork
and publish a real tagged release there.

## Landing site

`site/` is the project's landing page, built with [Hugo](https://gohugo.io) and the
[Hextra](https://github.com/imfing/hextra) theme. Hextra is a Hugo module pinned in
`site/go.mod`, a module of its own that the repository's `go build ./...` does not see; Hugo
fetches it with the `go` command, so Go must be installed. No Node.js is needed.

```sh
./tools/get-hugo.sh   # once: downloads the pinned, checksum-verified Hugo to tools/hugo
make site-serve       # preview at http://localhost:1313 (in memory), rebuilds on change
make site             # production build into site/public
```

The page is `site/content/_index.md`; its sections are small shortcodes under
`site/layouts/_shortcodes/landing/`, and `site/assets/css/custom.css` holds the page's own
styles (Hextra keeps the navbar, footer, theme switch and code blocks). Nothing is copied into
`site/`: module mounts in `site/hugo.yaml` serve the screenshots from `docs/images/`, the logo
from `internal/web/static/favicon.svg` and the UI's fonts from `internal/web/static/fonts/`.
Every screenshot exists as `<name>-light.png` and `<name>-dark.png`, taken from a local Krill
with sample data in each theme; the page shows the one matching the active theme. Links to the
documentation point at the Markdown files on GitHub. Keep the page's claims to what the README
and the guides already say. The one file kept in `site/static/` is `og.png`, the 1200×630 link
preview (`params.images` in `site/hugo.yaml`): the logo, the tagline and a crop of
`overview-dark.png`, rendered in a browser from a small HTML page; redo it when the tagline
changes. `site/static/video/` holds one short video per phase of the "From a fresh VPS to a cluster"
section (install, deploy, connect, expose, grow, observe), each as a light and a dark take in
WebM (VP9) and MP4 (H.264) with a JPEG poster. They are frames captured from a local Krill with
sample data and put together with ffmpeg; the install phase replays a real `install.sh` run and
the expose phase ends on curl's real answers. Addresses and passwords in them are masked. The
section's tabs are a small inline script in `landing/phases.html`: without script every phase is
shown in turn, and under `prefers-reduced-motion` nothing plays or advances on its own.

[`pages.yml`](../.github/workflows/pages.yml) builds the site and deploys it to GitHub Pages on
every push to `master` that touches it (or by hand); it takes `baseURL` from the Pages settings,
so a custom domain needs no config change. Pages has to be enabled once under
**Settings → Pages → Source: GitHub Actions**.

## CI and releases

- [`ci.yml`](../.github/workflows/ci.yml) runs on every push and pull request: `go build`,
  `go vet`, `go test`, a Docker image build without pushing, and a check that generated code
  is up to date. A change that touches only the landing site skips it.
- [`release.yml`](../.github/workflows/release.yml) runs on a `v*.*.*` tag. It pushes a
  multi-arch image to `ghcr.io/proshik/krill` (tags without the leading `v`, plus `latest`;
  the Dockerfile cross-compiles the binary for each platform rather than emulating it) and
  creates a GitHub Release with static `krill-linux-amd64` / `krill-linux-arm64` binaries,
  `krill-cli` tarballs for Linux and macOS, and `checksums.txt` — the files `install.sh`
  downloads and verifies.

```sh
git tag v0.2.0 && git push origin v0.2.0
```

To check the image locally: `docker build -t krill:local .` The build uses the committed
generated files and does not run `make generate`.

## Tasks

Open work is tracked as tasks in [`tasks/`](../tasks/), in the
[taskmoor task format](https://github.com/proshik/taskmoor/blob/main/spec/README.md)
(`format = 1`). [ROADMAP.md](../ROADMAP.md) keeps the summary of what is done and an index of
the open topics; the task files hold the details.

- **One task, one merge.** A task is one file, `tasks/KRL-<n>-<slug>.md`: a YAML front matter
  (`id`, `title`, `kind`, `status`, `priority`, `component`, `depends_on`, `touches`,
  `harness`, `reviews`, `created`, and optionally `needs_person`, `after_release`) and a body
  with **Goal**, **Acceptance** (a checklist of WHEN/THEN items), **Checks**,
  **Limits / non-goals** and **Context**. It is finished in one worktree, on one branch, by one
  merge. A task too big for that is split before it starts.
- **Written from the code.** A task states facts with a path and a line where it can, and says
  what its author has not verified. It is self-contained: the maintainer's local-only notes
  under `docs/superpowers/` may be cited as context, never relied on.
- **Configuration.** [`tasks/tasks.toml`](../tasks/tasks.toml) sets the prefix `KRL`, the main
  branch `master` and the components (`krill`, `site`, `docs`, `tooling`) with their release
  rule: a `krill` task is `done` once a `v*.*.*` tag contains its merge, the others as soon as
  they are merged. Ids are never reused; a dropped task gets `status: cancelled`, it is not
  deleted.
- **Live acceptance** of something that needs a real server or cluster is a `research` task
  with `needs_person: true`; it records what was run and seen, with neutral host names.
- **Taking a task.** `taskmoor ready` lists the tasks whose dependencies are merged and whose
  `touches` do not overlap a task in progress or in review. Work on a branch
  `task/KRL-<n>-<slug>` in its own worktree, recorded with
  `taskmoor claim KRL-<n> --branch <branch> --worktree <path>`. Tick the Acceptance boxes in
  the task file on the branch as they pass; a disagreement with the task is written into the
  file, not coded around. `taskmoor validate` must report no errors.
- **Merging.** The branch is merged into `master` with a merge commit whose message ends with
  the trailer

  ```
  Task: KRL-<n>
  ```

  — that commit on `master`'s first-parent history is what makes the task `merged`. One such
  commit per task; a commit on the branch that mentions the id does not count.
