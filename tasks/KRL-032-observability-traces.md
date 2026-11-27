---
id: KRL-032
title: Observability stage 3 — ship apps' OTLP traces through the node agent
kind: feature
status: backlog
priority: 4
component: krill
depends_on: []
touches:
  - internal/observability/**
  - internal/deploy/**
  - internal/server/**
  - internal/web/**
  - internal/database/migrations/**
  - internal/database/queries/**
  - internal/database/gen/**
  - docs/guides/observability.md
  - docs/internals/operations.md
  - docs/configuration.md
harness: any
reviews: {spec: 1, code: 2}
created: "2026-11-27"
---

## Goal

Apps that emit OpenTelemetry traces send them over OTLP to the Alloy node agent Krill
already runs on every node (stage 1: host metrics and container logs; stage 2: app
metrics through the collector), and Alloy forwards them to a trace store the
operator provides. Krill keeps none of it, as in the earlier stages.

## Acceptance

- [ ] WHEN the operator sets a trace receiver (OTLP endpoint and optional
      authentication) in Settings → Observability THEN the agent's config gains an
      OTLP receiver and exporter, the credential is stored with `secret.Enc` and
      reaches Alloy the way the existing receiver secrets do (a 0400 root-owned file,
      never Env or argv).
- [ ] WHEN an app opts in THEN its deploy injects the standard
      `OTEL_EXPORTER_OTLP_ENDPOINT` pointing at the agent reachable from the app's
      organization network, and nothing changes for apps that do not opt in (the
      fingerprint of their spec is unchanged).
- [ ] WHEN traces arrive THEN they carry `krill_*` resource attributes (organization,
      project, environment, app, node) like the logs' labels.
- [ ] WHEN the receiver is unreachable THEN the agent buffers within a bound and drops
      beyond it; the app is never slowed or failed by it.
- [ ] WHEN the docs are updated THEN `docs/guides/observability.md` explains the
      setup and `docs/internals/operations.md` the design.

## Checks

- An integration test running the real Alloy agent with the generated config and a
  local OTLP receiver (`alloy validate` alone proves little — see AGENTS.md).
- `make generate`, `make test`, `make lint-migrations`.

## Limits / non-goals

- No trace storage or viewer in Krill.
- One spec review round first: how the agent is reachable from every organization's
  network without joining tenants together is the open design question.

## Context

- `internal/observability` (`config.go`, `apps_module.go`, `reconcile.go`); the stage-2
  collector: `docs/guides/metrics.md`, `docs/architecture.md#application-metrics-collector`.
- AGENTS.md pitfalls on Alloy (`body_size_limit` units, `poll_timeout` below
  `poll_frequency`).
