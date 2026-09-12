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
- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/):
  `feat(scope): …`, `fix(scope): …`, `docs: …`.

[CLAUDE.md](CLAUDE.md) holds the detailed notes on internals and known pitfalls, written for AI
coding agents and just as useful to people.

## Pull requests

- One topic per pull request, with what changed and why.
- `make test` passes, and `make generate` leaves no diff.
- Behavior changes come with tests.
- User-visible changes update the docs in `docs/` or the README.
