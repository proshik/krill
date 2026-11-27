---
id: KRL-025
title: Direct image upload from krill-cli for users without a registry (delivery upload)
kind: feature
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/krillcli/**
  - cmd/krill-cli/**
  - internal/api/**
  - internal/server/**
  - internal/docker/**
  - internal/config/**
  - docs/guides/krill-cli.md
  - docs/configuration.md
  - docs/internals/agent-api.md
harness: any
reviews: {spec: 1, code: 2}
created: "2026-11-27"
---

## Goal

A developer with no registry account deploys an image built on their machine by
streaming it to Krill (`delivery: upload` in `krill.yaml`, `krill-cli deploy
--upload`). Today the mode is designed but refused at pre-flight, and the client's
`upload` hook has no caller (`internal/krillcli/client/client.go:35`).

## Acceptance

- [ ] WHEN `KRILL_IMAGE_UPLOAD_ENABLED` is unset THEN the endpoint does not exist (404)
      and the CLI says the server has upload turned off; the flag defaults to off.
- [ ] WHEN a write token sends `POST /api/v1/apps/{ref}/image?tag=<tag>` with a
      `docker save <image-id>` archive THEN Krill spools it to a directory on real
      disk (not `/tmp`, which may be tmpfs; Krill has no data-directory setting today,
      so the spec chooses one and documents it), and accepts it only if it holds
      exactly one image and **no** `RepoTags`; anything else is refused before
      `docker load` — a tagged archive could otherwise replace an image such as
      `traefik:v3.6.1` that Krill later runs as root with the Docker socket.
- [ ] WHEN the archive is accepted THEN the image is tagged
      `krill.invalid/<app-scoped name>:<tag>` (the reserved `.invalid` name, so
      swarmkit's pull on every task Prepare never leaks to Docker Hub) and deployed
      through the normal queue with that tag in the job (`Deployer.EnqueueImage`).
- [ ] WHEN the compressed or the unpacked size exceeds its own limit THEN the upload
      is refused (gzip bomb), the spool is removed, and nothing is loaded.
- [ ] WHEN the app is placed on a worker or is `global` THEN the upload is refused: an
      uploaded image exists only on the control plane.
- [ ] WHEN old uploaded images pile up THEN they are pruned without `Force` (an image a
      service still uses stays).
- [ ] WHEN the CLI runs `deploy` with `delivery: upload` THEN it builds, saves by image
      id, streams with progress, and follows the deploy as the registry path does; the
      `PLANNED` texts in `cli/root.go` and `cli/init.go` are replaced.
- [ ] WHEN the docs are updated THEN `docs/guides/krill-cli.md`, `docs/configuration.md`
      (the new variable) and `docs/internals/agent-api.md` describe the mode and its
      single-node limit.

## Checks

- Unit: archive inspection (tags present, two images, `manifest.json` last, oversize),
  name building, the placement guard, CLI argv and the drift test against `internal/api`.
- Integration (`-tags=integration`): an upload on a local Swarm deploys and serves; a
  tagged archive is refused and no image appears in `docker images`.
- `make test`.

## Limits / non-goals

- No layer de-duplication: every upload ships the whole image.
- No multi-node distribution of uploaded images.
- The `docker.Engine` interface is not widened: the load path is an optional
  capability found by type assertion, like the remote-exec one.

## Context

- `internal/krillcli/cli/deploy.go` (`--upload`, `project.DeliveryUpload`),
  `internal/krillcli/client/client.go:35-45` (the unused upload hook and its timeouts).
- The three constraints above (untagged single image, `krill.invalid/`, single-node
  guard) come from the maintainer's local CLI design (§8 "Direct image upload" of the
  2026-09-06 krill-cli spec, local-only); they are restated here in full.
