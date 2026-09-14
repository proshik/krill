# Configuration

Krill reads its configuration from `KRILL_*` environment variables. The installer writes
them to `/etc/krill/krill.env`; after editing that file, run `systemctl restart krill`. For
local development, see [`.env.example`](../.env.example).

Durations use Go syntax (`30s`, `15m`, `48h`); sizes use human units (`256m`, `1g`, `5gb`).

## Required

| Variable | Purpose |
|----------|---------|
| `KRILL_DATABASE_URL` | PostgreSQL connection string for Krill's own state. |
| `KRILL_ADMIN_EMAIL` | Email of the instance administrator, created on startup if missing. |
| `KRILL_ADMIN_PASSWORD` | That administrator's initial password. |

## Server and URLs

| Variable | Default | Purpose |
|----------|---------|---------|
| `KRILL_LISTEN_ADDR` | `:8080` | HTTP bind address of the Krill UI and API. |
| `KRILL_HOST` | `localhost` | Public hostname used in external database connection strings. |
| `KRILL_PUBLIC_URL` | derived | Externally reachable base URL, shown in webhook URLs. Defaults to `https://<panel domain>` once one is confirmed, otherwise to `scheme://KRILL_HOST`, with the scheme taken from `KRILL_COOKIE_SECURE`. |
| `KRILL_BASE_DOMAIN` | `127-0-0-1.sslip.io` | Suffix of each app's auto-generated domain. |
| `KRILL_DOCKER_HOST` | — | Docker daemon address, e.g. a Colima socket. Empty uses the Docker default. |
| `KRILL_NETWORK` | `krill-net` | Traefik's default provider network, and the fallback for an organization not yet moved onto its own `krill-org-<id>` network. |
| `KRILL_ADVERTISE_ADDR` | — | The manager's Swarm address, an IP or a hostname. Workers join the cluster at it, the cluster firewall keeps it allowed, and the gateway reaches the Krill UI at it when the UI has a [domain](guides/domains.md#serve-the-krill-ui-on-a-domain). Adding a worker, locking down the firewall and the panel domain all need it. The installer detects and writes it. |
| `KRILL_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `KRILL_LOG_FORMAT` | `text` | `text` or `json`. |

## TLS

| Variable | Default | Purpose |
|----------|---------|---------|
| `KRILL_ACME_EMAIL` | admin email | Let's Encrypt contact. |
| `KRILL_ACME_STAGING` | `false` | Use the Let's Encrypt staging CA while testing, to stay clear of rate limits. |

## Security

| Variable | Default | Purpose |
|----------|---------|---------|
| `KRILL_SECRET_KEY` | — | Encrypts stored secrets at rest with AES-256-GCM: database passwords, registry, Git and storage credentials, webhook secrets, notification tokens, node SSH keys. The installer generates one. Empty stores them in plaintext and logs a warning at startup. **Changing the key makes existing secrets unreadable.** |
| `KRILL_COOKIE_SECURE` | `false` | Force the `Secure` flag on every cookie. **Not needed for the [panel domain](guides/domains.md#serve-the-krill-ui-on-a-domain)**: requests that arrive through it over HTTPS get secure cookies anyway. Turn it on only when a TLS proxy of your own fronts Krill; it breaks sign-in over plain `http://<ip>:8080`, and the sign-in page says so. HSTS for the panel domain is not an environment variable: it is set on the Panel domain page. |
| `KRILL_TRUST_PROXY` | `false` | Take the client IP from `X-Forwarded-For` for login rate limiting on every request. Enable **only** behind a reverse proxy of your own that sets the header — otherwise clients can forge it. Requests through Krill's own gateway are recognized without it. |
| `KRILL_ALLOW_PRIVATE_EGRESS` | `false` | Allow outbound connections to private, loopback and link-local addresses for S3 storage, registries and Git clones. Off by default as SSRF protection; enable it for a MinIO, registry or Git server on your own network. |

## Deploys and builds

| Variable | Default | Purpose |
|----------|---------|---------|
| `KRILL_DEFAULT_MEMORY_LIMIT` | `512m` | Memory limit for any app or database instance without one of its own. Empty disables the default. |
| `KRILL_DEFAULT_CPU_LIMIT` | `1.0` | CPU limit, in cores, for any app or database instance without one of its own. Empty disables the default. |
| `KRILL_MAX_BUILDS_PER_ORG` | `2` | How many deploys one organization may have in flight at once, so one tenant can't hold the shared build queue. `0` disables the cap. |
| `KRILL_BUILD_CACHE_LIMIT` | `5gb` | BuildKit cache size kept by the periodic `docker builder prune`. Empty disables pruning. |
| `KRILL_BUILD_PRUNE_INTERVAL` | `24h` | How often the build cache is pruned. `0` disables pruning. |
| `KRILL_CONVERGE_TIMEOUT` | `180s` | How long a deploy waits for new tasks to run, extended by a healthcheck's start period. |
| `KRILL_MIGRATE_TIMEOUT` | `30m` | Cap on moving a database instance's volume to another node. |

## Operations

| Variable | Default | Purpose |
|----------|---------|---------|
| `KRILL_HEALTH_POLL_INTERVAL` | `30s` | How often service health is checked for down/recovered alerts. |
| `KRILL_TERMINAL_IDLE_TIMEOUT` | `15m` | Closes an idle web-terminal session. `0` disables it. |
| `KRILL_METRICS_INTERVAL` | `30s` | How often CPU and memory are sampled. |
| `KRILL_METRICS_RETENTION` | `48h` | How long metric history is kept. |
| `KRILL_METRICS_NODE_TIMEOUT` | `10s` | Per-worker timeout when collecting stats over SSH. |

## Agent API

| Variable | Default | Purpose |
|----------|---------|---------|
| `KRILL_AGENT_API_ENABLED` | `true` | Enables **both** the REST API (`/api/v1`) and the MCP server (`/mcp`). `false` turns both off. |
| `KRILL_MCP_SESSION_TIMEOUT` | `30m` | Closes an idle MCP session that its client never ended. `0` disables it. |
