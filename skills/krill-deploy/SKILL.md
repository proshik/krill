---
name: krill-deploy
description: Use when shipping an application to Krill or working out why one there is broken — deploy a change and verify the rollout, restart or stop a service, check what is running, or diagnose a failed deploy or a service that is down. Applies whenever the app in question is hosted on a Krill instance reachable through the krill MCP server.
---

# Deploying and diagnosing apps on Krill

Krill exposes twenty tools. Sixteen **read and operate what already exists**; four more, for a token issued at **provision** level, **create** a logical database, an image app, a domain or a database link. Nothing deletes or changes an existing organization, project, environment, app, database, domain or volume, and there is no shell into a container. Anything outside that list needs a human in the Krill web UI — say so rather than looking for a workaround.

| Tool | Level | What it does |
|---|---|---|
| `krill_whoami` | read | Who the token acts as, which org, whether it may write, and which installation answered (`instance.url`, `instance.version`) |
| `krill_list_apps` | read | Every app in the org: path, id, status, source type, image repository + tag, domains |
| `krill_app_status` | read | One app: status, `running/desired` replicas, node, domains, last deployment; `image` is the repository, `tag` the configured tag. Also, when Swarm answers: recent `tasks` (node, state, error, exit code), `failed_since_deploy`, container `health`, `usage` (memory, CPU when attributable) |
| `krill_app_logs` | read | Runtime log, parsed, most recent lines across all tasks; `tail` (default 200, max 1000), a minimum `level` (lines with no detected level always pass), a window `since`/`until` (RFC3339 or `10m`), `contains` (ignores case). With `wait_for` (+ `timeout`, max 120 s) it waits for a **new** line instead — written after the call (`following_since`); it does not search earlier lines and takes no `since` |
| `krill_deployments` | read | Deployment history, newest first |
| `krill_deployment_status` | read | One deployment: status plus the tail of its build log; `wait` (max 120 s) answers when it ends; `settling: true` = applied, Swarm still converging |
| `krill_list_env` | read | Environment variable **names** and their source — never values |
| `krill_app_config` | read | Saved settings without secrets: source, resources, healthcheck timing, ports, volumes, domains, links, and `pending` sections saved but not applied yet |
| `krill_list_backups` | read | Configured backups (database, volume, bucket): schedule, `running`, last run and its status/error, last success; filter by `app` or `environment` |
| `krill_domain_status` | read | Per domain: DNS answer, whether it points at the gateway, and the certificate the gateway serves (`trusted`, `default_cert`, `days_left`) |
| `krill_deploy` | write | Queue a deploy; optional `tag` retags an `image` app first |
| `krill_rebuild` | write | `--no-cache` build, `dockerfile` apps only |
| `krill_reload` | write | Restart the running tasks in place — same image, no build, no pull. A stopped app is deployed instead (`action: "deploy"` + a deployment id to poll) |
| `krill_stop` | write | Scale to zero replicas, cancelling a deploy that has not applied yet (a tag it carried stays the app's tag); `krill_deploy` brings it back |
| `krill_set_env` | write | Set, add or remove one variable, leaving the rest of the file untouched |
| `krill_backup_now` | write | Start a configured backup now (`kind` + `id` from `krill_list_backups`); asynchronous, answers `started_at` |
| `krill_create_database` | provision | Create a logical Postgres database in `environment` (`project/env`) on an existing `server` |
| `krill_create_app` | provision | Create an image app in `environment` — not deployed; primary `domain` optional (default internal, no HTTPS) |
| `krill_add_domain` | provision | Add a domain to an app: `exposed`, `tls`, `paths` |
| `krill_link_database` | provision | Inject a database's connection value into one variable (`database` in the app's environment, or a non-Postgres `server`) |

An app is addressed as `project/environment/app` or by numeric id — `krill_list_apps` returns both. If more than one app plausibly matches what the user named, ask which; do not guess.

**Several Krill installations.** App ids repeat across installations, so an id that is right on one is a different app on another. When more than one Krill is reachable (several MCP servers, or MCP plus REST), check `krill_whoami`'s `instance.url` against the installation the user means **before every write**, and address apps by `project/environment/app` path rather than by id. The URL identifies an installation only when it is its own public address: if two report the same one (an unconfigured `http://localhost`, say), you cannot tell them apart this way — ask the user which one before writing.

## Deploy and verify

1. `krill_list_apps` — find the app, note its `source_type` and current `status`.
2. If it is a production environment, **ask before deploying** (rule 4).
3. Pick the operation that matches the change:
   - **`krill_deploy`** for a normal deploy. An `image` app takes `tag` to move to a new tag; a `dockerfile` app is cloned and built with layer cache.
   - **`krill_rebuild`** when the build must not reuse cached layers (`dockerfile` apps only).
   - **`krill_reload`** when nothing was rebuilt and the process just needs a bounce.
4. `krill_deploy` and `krill_rebuild` are asynchronous: they return a `deployment_id` right away and the build runs on the server. Wait with `krill_deployment_status` and `wait: 120` (rule 1); repeat while it still says `running`. `settling: true` means the spec is applied and Swarm is still rolling it out — not done yet.
5. On `done`, verify with `krill_app_status` that replicas read `n/n` and the status is `running`, and that `failed_since_deploy` is 0 and `health` (if any) is `healthy`. A green deployment with `0/1` replicas or failed tasks means the container started and died — go to the diagnose branch. To see the app's ready line (for example `listening`), look in three steps, so the gaps between calls are covered:
   1. `krill_app_logs` with `contains: "listening"` and `since` = the deployment's `started_at`. Found — go on.
   2. Otherwise `krill_app_logs` with `wait_for: "listening"` and a `timeout` (up to 120). It only watches lines written after it started (`following_since`), and possibly not the first moments after that.
   3. If the wait ends without a match — `timeout`, `ended` or `error` — step 1 once more: it covers the moments between steps 1 and 2.
   Still nothing is inconclusive, not a failure, when the last read ended with a "log read incomplete" line or the wait said `incomplete`: say so and fall back on the status. A generic line can also come from the **previous** release's task during a rollout (`started_at` is when the deploy was queued): treat it as supporting evidence, and prove the new release with a line only it prints (its version) or with the status — replicas `n/n`, `failed_since_deploy` 0, `health` healthy.
6. On `error`, read the `log_tail` the same call returned, name the cause, and stop (rule 2).

`krill_set_env` writes the variable but does **not** restart anything: the app keeps running the old value until the next deploy. If the change is meant to take effect, deploy after it — and say that you are about to.

## Set up a new app

With a provision-level token: `krill_create_database` (if it needs one) → `krill_create_app` → `krill_link_database` for the connection variable → `krill_set_env` for each variable the user gave you a value for → `krill_add_domain` (or the app's own `domain`) → `krill_deploy`. Every create is idempotent: repeating one that already worked returns `created: false`, so a retry after a timeout is safe. A `conflict` means something with that name already exists **with other settings** — report it, do not pick another name on your own. Exposing a domain to the internet and turning on HTTPS are production changes: ask first (rule 4).

## When the image has to be built locally

`krill_deploy` moves an `image` app to a tag that **already exists in a registry**. It cannot build one: the image bytes live on the user's machine, and no MCP tool can carry them.

So when the user's change is source code that has not been built and pushed yet, the deploy tool is not the answer — `krill-cli` is. If it is installed and you are working in the application's own repository:

1. Check there is a `krill.yaml` (`krill-cli` needs one; `krill-cli init` writes it).
2. Run `krill-cli deploy` through the shell. It builds locally, pushes, deploys and waits, and it verifies the token, the app and the image repository *before* the build.
3. Read its exit code rather than its prose: `0` ok · `1` the deployment failed · `2` configuration, arguments or a missing app · `3` another deploy was in flight (only with `--no-wait-for-lock`; by default it waits) · `4` timed out watching, **or the outcome could not be read** · `5` deployed but not running · `6` authentication. Codes `4` and `5` both mean "do not assume this worked" — check with `krill_app_status` before reporting anything.
4. Verify with `krill_app_status` as usual.

Two things this branch does not change. **Rule 4 still applies** — building and shipping arbitrary local code to production is a bigger action than moving a tag, not a smaller one, so ask first. And if `krill-cli` is not installed, or the shell is not in the application's repository, say so and stop; do not hand-roll `docker build` and `docker push` to work around it.

## Diagnose a failure

1. `krill_app_status` — is it `running`, `error`, `idle`? What do replicas say, and when was the last deployment? Read `tasks` the way you would read `docker service ps`: a task `failed` with an `error` and `exit_code` (137 is usually out of memory — compare `usage.mem_bytes` with `mem_limit_bytes`), `rejected` (the node refused it: an image that does not pull, a missing volume or constraint), `starting` that never turns `running` (its healthcheck fails — see `health`, `failing_streak`). `failed_since_deploy > 0` is a crash loop since the last deploy.
2. `krill_deployments` — did the failure start with a deploy? Line the timestamps up against when the user says it broke. A service that has been down since a deploy and one that died on its own point at different causes.
3. `krill_deployment_status` on the suspect deployment — the build log tail usually names a failed build outright.
4. `krill_app_logs` with `level: "error"` first; if that is empty or unhelpful, widen to `info` and raise `tail`. A line with no detected level (`lvl: ""`) always passes the filter, and most unstructured output has none — so on such an app the result is not "all errors"; read the lines. For "what happened around 14:05", use `since`/`until` and `contains` instead of a large `tail`. A line longer than 16 KB is stored by Docker in pieces, and a search text that falls across two pieces is not found — a miss on such output is not proof. Empty logs at `0/n` replicas usually mean the container never got far enough to log.
5. `krill_list_env` when the logs point at configuration — a missing variable is a finding worth reporting (rule 3). `krill_app_config` shows the rest of the settings (resources, healthcheck timing, ports, volumes, domains, links) and `pending` — saved settings the running deployment does not have yet.
6. A domain that does not answer or answers with a browser warning: `krill_domain_status`. `points_at_gateway: false` means DNS answers with none of the addresses Krill knows for its gateway (its advertised address and the node its gateway runs on) — before calling it a wrong record, check with the user which public address should receive the traffic: behind NAT, a VPC or WireGuard, the public ingress can differ from every address Krill knows; `default_cert: true` means no certificate was issued for the host yet (DNS not pointing here, or the domain was added moments ago); `trusted: false` with a real issuer is worth reading `verify_error`.
7. Before a risky change to data: `krill_list_backups` for the app — is there a backup, is it `enabled`, when did it last succeed? To take one now, `krill_backup_now` (ask first in production, rule 4 — and say that the run applies the backup's retention, so it can delete the oldest stored copy), then `krill_list_backups` until `running` is false and check that `last_run_at` equals the `started_at` you got (earlier: this run recorded nothing — treat it as failed) and `last_status` is `ok`. `last_error` is Krill's summary; the full error is in the web UI.

Finish with a hypothesis and the evidence for it: which deployment, which log line, what you think happened. Propose the fix; do not silently apply one.

## Four rules

**1. Wait on the server, do not poll.** Call `krill_deployment_status` with `wait: 120`: it answers as soon as the deployment ends, or after two minutes with the state at that moment; repeat while it says `running`, and stop at about ten minutes — report where it got to and hand back. The same goes for a log line: `krill_app_logs` with `wait_for` (after a `contains` read, as in step 5 above), not a loop of log reads. Polling every few seconds fills your own context with identical "running" answers and evicts the task you were called for, and it burns the per-token budget of 60 requests/minute, which the user's other calls share; a waiting call costs one request however long it waits.

**2. Do not retry a failed deploy.** Nothing changed between the two attempts, so the same code fails the same way, and the second identical failure buries the first one's cause under a fresh log. Pull the log tail, name the cause, stop. Retry only after the user has changed something — and then say what changed.

**3. Never invent an environment variable's value.** The tools return names, not values, deliberately: everything you read reaches the model provider. You cannot see what `DATABASE_PASSWORD` is, so you cannot restore it, and a plausible-looking value written over a real one breaks the app quietly. Report a missing or suspect variable; set one only with a value the user gave you.

**4. Production deploys ask first.** One explicit confirmation before deploying, rebuilding, stopping or reloading anything in a production environment. Separately: a read-level token cannot write at all, and rights are re-resolved on every call from the token's level and its owner's current role — so check `krill_whoami` (`can_write`) before a write instead of discovering it from a rejection.

## Errors

Error messages are written to be acted on — "this is a dockerfile app, use rebuild" rather than "invalid argument". Read them before changing approach. An app in another organization reports as *not found*, not *forbidden*, so a not-found on an app the user insists exists means the token is scoped to a different org: check `krill_whoami`.
