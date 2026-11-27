---
id: KRL-041
title: SSH deploy keys for cloning private Git repositories
kind: feature
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/builder/**
  - internal/deploy/**
  - internal/server/**
  - internal/web/**
  - internal/database/migrations/**
  - internal/database/queries/**
  - internal/database/gen/**
  - internal/statebackup/**
  - docs/guides/deployments.md
  - docs/internals/deploy.md
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-27"
---

## Goal

A private repository is cloned today over HTTPS with a token fed through a
`GIT_ASKPASS` helper. Many teams prefer a read-only deploy key per repository. An
app with an `ssh://` or `git@host:` URL is cloned with a key Krill generates; the
user adds its public half to the repository.

## Acceptance

- [ ] WHEN an org admin turns on a deploy key for a Git app THEN Krill generates an
      ed25519 key pair, stores the private key with `secret.Enc`, and shows only the
      public key (to paste as a read-only deploy key).
- [ ] WHEN the app builds from an SSH URL THEN the clone uses that key through a
      temporary file (0600, removed after the clone) and `GIT_SSH_COMMAND` with
      `IdentitiesOnly=yes`, never argv or the process environment of anything
      long-lived; the host key is pinned on first use per host and a change is refused
      with an error that says so.
- [ ] WHEN the key is bound to a host THEN it is used only for that host, like the
      existing credential host binding.
- [ ] WHEN the key is rotated THEN the old one stops being used at once and the UI shows
      the new public key.
- [ ] WHEN the state backup runs THEN the encrypted private key is treated like the
      other stored credentials (check `statebackup.DumpArgs()` for whether the table's
      data is excluded).
- [ ] WHEN the docs are updated THEN `docs/guides/deployments.md` and
      `docs/internals/deploy.md` describe it.

## Checks

- Unit: URL classification (https / ssh / scp-like), command building, the key never
  in logs or errors.
- Integration: clone from a local SSH Git server container with a deploy key; a wrong
  host key is refused.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- No GitHub App (KRL-040); no SSH agent forwarding.

## Context

- `internal/builder/git.go` (`sanitizeGitURL`, `GIT_ASKPASS` helper, `KRILL_GIT_PASSWORD`),
  `internal/builder/builder.go` (`Token`); credential host binding in
  `docs/internals/deploy.md`.
