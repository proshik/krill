---
id: KRL-024
title: Re-create missing logical databases and their users when a Postgres instance is deployed
kind: feature
status: backlog
priority: 2
component: krill
depends_on: []
touches:
  - internal/dbservice/**
  - internal/web/i18n/**
  - docs/install.md
  - docs/internals/databases.md
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-27"
---

## Goal

A Postgres instance deployed onto an empty volume — a new host after a state-backup
restore, a lost node — gets back every logical database Krill has in its catalog,
with its owner user and password, so the restore runbook no longer needs a manual
`CREATE USER` / `CREATE DATABASE` / `REVOKE CONNECT` per database (step 8 of
runbook A in `docs/install.md`). Objects that already exist are never touched.

## Acceptance

- [ ] WHEN a Postgres instance is deployed and has started accepting connections THEN
      for each of its `logical_databases` rows the database and the role are created
      only if neither exists, with the stored password (`secret.Dec`) and the same
      statements as `ProvisionLogicalDB` (including `REVOKE CONNECT … FROM PUBLIC`).
- [ ] WHEN a database or role of that name already exists THEN nothing is created,
      altered or dropped for that row (the existing data wins), and the deploy log
      says so for that database by name.
- [ ] WHEN only one of the pair exists (a role without its database, or the reverse)
      THEN the row is left alone and reported as needing an operator's attention, with
      no compensation drop of anything.
- [ ] WHEN a stored password cannot be decrypted (`secret.ErrUndecryptable`) THEN that
      database is skipped with a logged error and the others still proceed; the
      deploy itself is not failed by a re-provision error.
- [ ] WHEN the re-provision runs THEN it holds the instance lock the deploy already
      holds, and runs on a context detached from the HTTP request.
- [ ] WHEN the docs are updated THEN runbook A in `docs/install.md` drops the manual
      SQL step (or keeps it only for releases before this one), and
      `docs/internals/databases.md` describes the behavior.

## Checks

- Unit: the per-row decision (both missing → create; both exist → skip; one exists →
  report; undecryptable → skip) with a fake engine.
- Integration (`-tags=integration`, Colima env from AGENTS.md): a real Postgres
  instance with two logical databases, its volume replaced by an empty one,
  redeployed → both databases accept their users' logins; a database created by hand
  under another owner is left untouched.
- `make test`.

## Limits / non-goals

- Postgres only (`Driver.HasLogicalResource()`); no data restore — the backups do that.
- No ALTER of an existing role's password: a role that exists is the operator's.
- No migration.

## Context

- `internal/dbservice/instance.go:160` (`deployCore`: pull → `ServiceDeploy` → status
  `running`, no wait for Postgres to accept connections).
- `internal/dbservice/provision.go` (`ProvisionLogicalDB`, `existsSQL`,
  `ErrObjectsExist` — never drop a database Krill did not create).
- `docs/install.md` ("Restore Krill's state from a backup", runbook A step 8).
