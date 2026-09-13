# Private images and repositories

## Private registries

To deploy an image from a private registry:

1. Open **Settings → Registries** (admin) and add one: a name, the registry URL (`ghcr.io`,
   `registry-1.docker.io` for Docker Hub, `registry.gitlab.com`, …), a username, and a password
   or token. Krill checks the credentials against the registry when you save them.
2. On the app — in the create dialog or on the **General** tab — choose that registry instead
   of "Public (no auth)".
3. Deploy. Krill hands the credentials to Swarm, which pulls the image with them on whichever
   node runs the app.

Notes:

- A credential is bound to its registry's host: Krill refuses to send it to any other
  registry, even if an app's image points elsewhere.
- One registry per app.
- Registries that issue short-lived tokens (AWS ECR, Google Artifact Registry) are not
  supported yet. A static username and token covers GHCR, Docker Hub and GitLab.
- A registry on a private address needs `KRILL_ALLOW_PRIVATE_EGRESS=true`.

## Private Git repositories

A Dockerfile app is built from a Git repository cloned over HTTPS. For a private one:

1. Open **Settings → Git credentials** and add a personal access token for the Git host.
2. Choose that credential on the app.

The token reaches `git` through a credential helper — never in the clone URL, the process
arguments or the build log — and, like a registry credential, is used only for the host it was
created for.

## Build arguments and secrets

On a Dockerfile app's **Advanced** tab:

- **Build arguments** are passed as `--build-arg` and may appear in the image history. Use them
  for non-secret values.
- **Build secrets** are passed through BuildKit's `--secret` and read in the Dockerfile with
  `RUN --mount=type=secret,id=<name>`. They are encrypted at rest, never written into the build
  context, and don't end up in the image.
