---
id: KRL-036
title: Serve an S3 server's web console on a domain through Traefik
kind: feature
status: backlog
priority: 4
component: krill
depends_on: []
touches:
  - internal/dbservice/**
  - internal/traefik/**
  - internal/hostshare/**
  - internal/server/**
  - internal/web/**
  - internal/database/migrations/**
  - internal/database/queries/**
  - internal/database/gen/**
  - docs/guides/buckets.md
  - docs/internals/databases.md
harness: any
reviews: {spec: 1, code: 2}
created: "2026-11-27"
---

## Goal

An S3 server's console (port 9001) is reachable today only through an external port
on the control plane (`console_external_port`, a `krill-dbproxy-<id>-console`
proxy), over plain TCP. An operator gives it a domain with Let's Encrypt HTTPS, and
the protections app domains already have (IP allowlist, basic auth), instead of
opening a raw port.

## Acceptance

- [ ] WHEN an org admin sets a console domain on an S3 server THEN Traefik routes it
      over the organization network to the instance's console port with a Let's
      Encrypt certificate, and the host is reserved through `internal/hostshare` like
      any app domain (no clash with apps or the panel domain).
- [ ] WHEN the domain has an IP allowlist or basic auth THEN they apply as on app
      domains.
- [ ] WHEN the domain is removed THEN the router is gone and the external port, if set,
      is unaffected.
- [ ] WHEN the migration runs THEN it is additive.

## Checks

- Integration against the pinned Traefik and the Silo image: the console answers on
  the host, another host does not reach it.
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- The S3 API itself on a domain is out of scope (apps reach it on the org network).

## Context

- `internal/dbservice/drivers/minio.go` (two external targets: S3 API 9000 and console
  9001), migration 000035 (`console_external_port`).
- Domains and labels: `internal/traefik/labels.go`, `docs/internals/networking.md`.
