---
id: KRL-042
title: Sign checksums.txt and verify the signature in the installer and the self-updater
kind: feature
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - .github/workflows/release.yml
  - install.sh
  - internal/selfupdate/**
  - docs/install.md
  - docs/internals/operations.md
  - SECURITY.md
harness: any
reviews: {spec: 1, code: 2}
created: "2026-11-27"
needs_person: true
---

## Goal

`install.sh` and the self-updater verify a downloaded binary against the release's
`checksums.txt` — but that file comes from the same place as the binary, so whoever
can replace one can replace both. A detached ed25519 signature of `checksums.txt`,
checked against a public key built into the installer and the binary, makes a
tampered release fail closed.

## Acceptance

- [ ] WHEN a release is tagged THEN the release job signs `checksums.txt` with a key
      held only as a repository secret and attaches the signature
      (`checksums.txt.sig`); the maintainer generates the key and stores the secret
      (`needs_person`).
- [ ] WHEN `install.sh` downloads a release THEN it verifies the signature with the
      embedded public key before trusting `checksums.txt`; a missing or bad signature
      aborts unless an explicit override is set, as with `KRILL_SKIP_VERIFY`. The tool
      used (`openssl`, `ssh-keygen -Y verify`, or a small verifier) is one a stock
      Ubuntu/Debian host has, and the choice is recorded.
- [ ] WHEN the self-updater installs a release THEN `internal/selfupdate` verifies the
      same signature with the key compiled into the running binary (Go's
      `crypto/ed25519`), and refuses an unsigned release once signing has shipped.
- [ ] WHEN signing is introduced THEN the order keeps the public one-liner working
      throughout: `master/install.sh` installs the latest release
      (`install.sh:321-325`), and release assets come from a separate tag workflow, so
      signed assets exist on the latest release before an enforcing installer reaches
      `master`; a `KRILL_VERSION` that selects a release from before signing is handled
      as documented (refused, or allowed with the override). The move from the last
      unsigned release is checked.
- [ ] WHEN the key must be rotated THEN the procedure (a release signed by the old key
      that carries the new public key) is written in `docs/internals/operations.md`,
      including a host that skipped that bridge release: the updater accepts only the
      cached latest tag (`checkStart`, `internal/selfupdate/updater.go:314-322`), so such
      a host is either offered the bridge release or told to re-run the installer, and
      the docs say how the operator trusts the new key.
- [ ] WHEN releases made before signing are offered THEN the transition is defined: the
      first signed release still accepts unsigned older ones for rollback, or not —
      the spec decides and the docs say.

## Checks

- Unit: signature verification (good, bad, missing, wrong key) in `internal/selfupdate`.
- A local run of the installer against a mirror with a good and a tampered signature.
- `make test`.

## Limits / non-goals

- No Sigstore/cosign keyless signing; the container image signing is separate.

## Context

- `install.sh` (checksum verification around lines 335–345, `KRILL_SKIP_VERIFY`),
  `internal/selfupdate/checksums.go` (`parseChecksums`), `checker.go`, `install.go`.
- `.github/workflows/release.yml` (builds `checksums.txt`, line ~111).
