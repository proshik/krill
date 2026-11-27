---
id: KRL-028
title: Publish krill-cli through a Homebrew tap on every release
kind: chore
status: backlog
priority: 4
component: tooling
depends_on: []
touches:
  - .github/workflows/release.yml
  - docs/guides/krill-cli.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-27"
needs_person: true
---

## Goal

A macOS or Linux user installs and upgrades `krill-cli` with `brew install
proshik/krill/krill-cli` instead of downloading a tarball by hand.

## Acceptance

- [ ] WHEN the maintainer has created the tap repository (`proshik/homebrew-krill`)
      and a token that may push to it only, stored as a repository secret THEN the
      release job renders `Formula/krill-cli.rb` from the release's
      `krill-cli_<version>_<os>_<arch>.tar.gz` assets and their SHA-256 from
      `checksums.txt`, and pushes it to the tap.
- [ ] WHEN a release is tagged THEN `brew install` of the new formula installs a
      binary whose `krill-cli version` reports the tag, on macOS arm64 and amd64 and
      on Linux.
- [ ] WHEN the tap push fails THEN the GitHub release itself is still published and
      the job reports the failure.
- [ ] WHEN the docs are updated THEN `docs/guides/krill-cli.md` shows the brew install.

## Checks

- `brew audit --strict` of the generated formula (on a runner or locally) and an
  install from the tap after the first release that publishes it.

## Limits / non-goals

- Creating the tap repository and the token are the maintainer's steps
  (`needs_person`); the agent never handles the token.
- No homebrew-core submission, no GoReleaser migration.

## Context

- `.github/workflows/release.yml` (krill-cli is built for macOS and Linux and packed
  as `krill-cli_${VERSION}_${os}_${arch}.tar.gz`, lines ~94–111; `checksums.txt` covers
  them).
