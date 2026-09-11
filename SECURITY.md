# Security Policy

## Supported versions

Krill is pre-1.0. Security fixes land on `master` and ship in the next release;
only the latest release is supported.

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Report it privately through GitHub instead: open the repository's **Security** tab
and choose **Report a vulnerability**
(<https://github.com/proshik/krill/security/advisories/new>).

A useful report includes the affected version or commit, the steps to reproduce,
and what an attacker gains. Krill has a single maintainer, so responses are
best-effort — expect an acknowledgement within a week. Once a fix is released the
advisory is published, with credit to the reporter unless you prefer otherwise.

## Operating Krill safely

Things worth knowing before you expose an instance — and before you file a report
about behaviour that is by design:

- **The control plane is the keys to the cluster.** It drives the Docker daemon
  and stores SSH credentials for every worker node, so a compromised control plane
  or instance-admin account means every node is compromised.
- **Organization roles are self-service.** Any signed-in user can create an
  organization and becomes its owner. Cluster-wide resources (nodes, host-wide
  monitoring) are therefore gated by a separate instance-admin flag held only by
  the seeded admin account, never by an organization role.
- **Set `KRILL_SECRET_KEY`.** Without it, stored credentials (database passwords,
  registry, S3, Git and SSH secrets, bot tokens) are kept in plaintext; the
  installer generates one for you.
- **Serve it over HTTPS** and set `KRILL_COOKIE_SECURE=true`. Set
  `KRILL_TRUST_PROXY=true` only behind a reverse proxy that sets
  `X-Forwarded-For` — otherwise clients can forge their IP for the login rate
  limiter.
- **The agent API (`/api/v1`, `/mcp`) is enabled by default.** Turn it off with
  `KRILL_AGENT_API_ENABLED=false` if you do not use it. API tokens and deploy-hook
  secrets are accepted only in the `Authorization` header, never in the URL.
