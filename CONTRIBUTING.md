# Contributing to Krill

Thanks for taking the time. Krill has a single maintainer, so reviews are best-effort — a
focused change with a clear description gets merged fastest.

## Before you start

- **Bugs and ideas:** open an [issue](https://github.com/proshik/krill/issues/new/choose). For
  anything bigger than a small fix, agree on the approach in an issue before writing code.
- **Security problems:** don't open a public issue — follow [SECURITY.md](SECURITY.md).

## Set up

[docs/development.md](docs/development.md) covers the prerequisites, running Krill from
source and the test setup. In short:

```sh
./tools/get-tailwind.sh   # once
make db-up
make run                  # with the variables from .env.example exported
make test
```

## Conventions

- **English everywhere** in code, comments, logs, errors and docs. The only exception is the
  Russian UI catalog.
- **UI text goes through i18n.** Use `i18n.T(ctx, "key")` and add the key to both
  `internal/web/i18n/en.go` and `internal/web/i18n/ru.go`.
- **Generated code is committed.** After changing a `.templ`, `.sql` or CSS file, run
  `make generate` and commit the output.
- **Tenancy is checked on every route.** A handler loads the organization → project →
  environment → resource chain and answers 404 on any mismatch, never 403. Keep the IDOR tests
  green and add one for a new route.
- **Errors are never swallowed** in handlers: log them or return a 500.
- **Secrets are never logged** — passwords, tokens, connection strings, env values.
- **Migrations must keep the previous release bootable.** Only additive changes (new tables;
  new columns that are nullable or have a `DEFAULT`; new non-unique indexes) are safe
  immediately. New unique indexes and foreign keys on existing tables are not additive, an index
  the previous release relies on for `ON CONFLICT` must never be dropped, and the previous binary
  must tolerate new enum or `CHECK` values the new one writes — widen reads a release before
  writes. Dropping/renaming a column or table, changing a column's type, or adding a `NOT NULL`
  column or constraint without a default may only ship one release after the code has stopped
  using the old shape (expand/contract) — self-update rolls back to the previous binary on a
  failed upgrade, and it must still start against the newer schema. CI checks new migrations
  with [squawk](https://squawkhq.com): run `./tools/get-squawk.sh` once, then
  `make lint-migrations` (rules in `.squawk.toml`). squawk catches the mechanical cases only;
  reviewers still read every migration, since it does not catch a new or tightened `CHECK`, enum
  widening, a dropped `ON CONFLICT` index or `CREATE UNIQUE INDEX` on an existing table. An
  intentional exception needs `-- squawk-ignore <rule>` on the line directly before the
  statement, followed on the same line by a comment with the reason and the release it is safe
  from: `-- squawk-ignore ban-drop-column -- safe from v0.5.0: unused since v0.4.0`.
- **Host-side upgrade steps must be idempotent startup code, not a manual step.** Anything
  `install.sh` would otherwise need to change on an upgrade — the systemd unit, the Postgres
  container, environment defaults — belongs in code the binary runs every time it starts, so a
  self-update never depends on the installer running again. If a release genuinely cannot avoid
  requiring the installer, its release notes must say so explicitly: "requires re-running
  install.sh".
- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/):
  `feat(scope): …`, `fix(scope): …`, `docs: …`.

[CLAUDE.md](CLAUDE.md) holds the detailed notes on internals and known pitfalls, written for AI
coding agents and just as useful to people.

## Pull requests

- One topic per pull request, with what changed and why.
- `make test` passes, and `make generate` leaves no diff.
- Behavior changes come with tests.
- User-visible changes update the docs in `docs/` or the README.
