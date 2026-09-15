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
  new columns that are nullable or have a `DEFAULT`; new indexes) are safe immediately.
  Dropping/renaming a column or table, changing a column's type, or adding a `NOT NULL` column
  or constraint without a default may only ship one release after the code has stopped using the
  old shape — self-update rolls back to the previous binary on a failed upgrade, and it must
  still start against the newer schema. Dropping a `CHECK` constraint is always fine; adding a
  new one is only safe if the same migration also drops a same-named one (the repo's
  drop-and-recreate-wider pattern), since the lint can't otherwise tell a widened re-creation
  from a tightened or brand-new one — it still takes a human reading the migration. `make test`
  lints this per statement (see `internal/database/compat.go`); an intentional exception needs a
  `-- krill:compat-break-ok <reason>` comment directly above that one statement.
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
