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
  - internal/server/db_handlers.go
  - internal/server/api_provision.go
  - internal/web/i18n/**
  - test/acceptance/**
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
- [ ] WHEN a logical database is being created through the UI or the API while the
      instance deploys THEN creation and re-provision are serialized on one per-database
      (or per-instance) lock that both entrypoints take from the catalog insert through
      provisioning — today both insert the row first and provision outside any instance
      lock (`internal/server/db_handlers.go:85-99`, `internal/server/api_provision.go:156-216`)
      — so neither side gets `ErrObjectsExist` for the other's objects, no row is deleted
      for objects that now exist, and no compensation drops objects the failing attempt
      did not create. Both completion orders are tested.
- [ ] WHEN the re-provision runs THEN it runs on a context detached from the HTTP
      request.
- [ ] WHEN the docs are updated THEN runbook A in `docs/install.md` replaces the
      unconditional manual SQL with: redeploy, then check each logical database and its
      role (login with the stored user) before restoring its dump, and run the manual
      statements only for a pair the deploy log reported as skipped or partial (kept for
      releases before this one); `docs/internals/databases.md` describes the behavior.
- [ ] WHEN the runbook changes THEN the state-backup acceptance follows it:
      `TestCheckRepoInstallDoc` (`test/acceptance/accept/runbook_test.go`, against the
      three-statement block `runbook.go` expects) and `recoverWorkloads` (`test/acceptance/statebackup_test.go:1038`,
      which runs the SQL with `ON_ERROR_STOP` and would fail on a role the deploy already
      recreated) check that the deploy recreated the database and role before the
      restore.

## Checks

- Unit: the per-row decision (both missing → create; both exist → skip; one exists →
  report; undecryptable → skip) with a fake engine.
- Integration (`-tags=integration`, Colima env from AGENTS.md): a real Postgres
  instance with two logical databases, its volume replaced by an empty one,
  redeployed → both databases accept their users' logins; a database created by hand
  under another owner is left untouched.
- `make test`; `make acceptance-statebackup` when the runbook changes (a disposable Lima
  VM, see `docs/development.md#state-backup-acceptance`).

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
