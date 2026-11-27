---
id: KRL-033
title: Move the Docker client off github.com/docker/docker v28 onto the moby client and api modules
kind: chore
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/docker/**
  - cmd/krill/main.go
  - internal/**/*_test.go
  - go.mod
  - go.sum
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-27"
---

## Goal

Krill imports the Docker client from `github.com/docker/docker v28.5.2+incompatible`.
All five advisories govulncheck reports against it are in daemon code, which the
split `github.com/moby/moby/client` and `github.com/moby/moby/api` modules do not
contain; on the old path they have no fix, and two of them
([GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887),
[GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883)) have no symbol list, so the
client import counts as calling them. Moving to the split modules clears the report
and keeps Krill on maintained code. Real exposure is the host's Docker Engine (fixed
in 29.3.1 and 29.5.1), which this task does not change.

## Acceptance

- [ ] WHEN the move is done THEN no `.go` file imports `github.com/docker/docker`, and
      `go.mod` lists `github.com/moby/moby/client` and `github.com/moby/moby/api` as
      direct requirements (today `moby/moby/api v1.54.1` is direct and
      `moby/moby/client v0.4.0` indirect).
- [ ] WHEN Krill talks to an older daemon THEN API version negotiation still works
      (the client negotiates down to API 1.40).
- [ ] WHEN the remote clients are built THEN `client.WithHost` still comes BEFORE
      `client.WithDialContext` in `NewRemoteClient`/`NewRemoteStats` (otherwise the SSH
      tunnel dialer is clobbered and the "remote" client talks to the local daemon).
- [ ] WHEN `govulncheck ./...` runs THEN none of the five advisories is reported.
- [ ] WHEN behavior is compared THEN Swarm deploys, exec, logs, stats, one-off
      containers, registry auth and SSH remote clients behave as before: the integration
      suite passes unchanged in what it asserts.

## Checks

- `make test` and `make test-integration` (Colima env from AGENTS.md), `govulncheck
  ./...`, a Docker image build (`docker run --rm krill:local` fails fast on the missing
  `KRILL_DATABASE_URL`).
- Measured on 2026-11-27: 29 files import the old path, 9 of them not tests:
  `cmd/krill/main.go` and `internal/docker/{client,logreader,objects,oneoff,predeploy,registry,release,servicelogs,spec}.go`.

## Limits / non-goals

- No behavior change; no new Docker features. Any type renamed by the new modules is
  adapted, not wrapped in a compatibility layer.

## Context

- `go.mod` lines 15, 21, 94; `internal/docker` (Swarm wrapper `Engine`, optional
  capabilities by type assertion).
- AGENTS.md pitfall "Remote docker clients: `client.WithHost` BEFORE
  `client.WithDialContext`".
