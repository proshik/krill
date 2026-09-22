---
name: krill-deploy
description: Use when shipping an application to Krill or working out why one there is broken — deploy a change and verify the rollout, restart or stop a service, check what is running, or diagnose a failed deploy or a service that is down. Applies whenever the app in question is hosted on a Krill instance reachable through the krill MCP server.
---

# Deploying and diagnosing apps on Krill

Krill exposes twelve tools. They **operate apps that already exist**: nothing here creates or deletes an organization, project, environment, app, database, domain or volume, and there is no shell into a container. Anything outside that list needs a human in the Krill web UI — say so rather than looking for a workaround.

| Tool | Level | What it does |
|---|---|---|
| `krill_whoami` | read | Who the token acts as, which org, and whether it may write |
| `krill_list_apps` | read | Every app in the org: path, id, status, source type, image repository + tag, domains |
| `krill_app_status` | read | One app: status, `running/desired` replicas, node, domains, last deployment; `image` is the repository, `tag` the configured tag |
| `krill_app_logs` | read | Runtime log tail, parsed, most recent lines across all tasks; `tail` (default 200, max 1000) and a minimum `level` (lines with no detected level always pass) |
| `krill_deployments` | read | Deployment history, newest first |
| `krill_deployment_status` | read | One deployment: status plus the tail of its build log |
| `krill_list_env` | read | Environment variable **names** and their source — never values |
| `krill_deploy` | write | Queue a deploy; optional `tag` retags an `image` app first |
| `krill_rebuild` | write | `--no-cache` build, `dockerfile` apps only |
| `krill_reload` | write | Restart the running tasks in place — same image, no build, no pull. A stopped app is deployed instead (`action: "deploy"` + a deployment id to poll) |
| `krill_stop` | write | Scale to zero replicas; `krill_deploy` brings it back |
| `krill_set_env` | write | Set, add or remove one variable, leaving the rest of the file untouched |

An app is addressed as `project/environment/app` or by numeric id — `krill_list_apps` returns both. If more than one app plausibly matches what the user named, ask which; do not guess.

## Deploy and verify

1. `krill_list_apps` — find the app, note its `source_type` and current `status`.
2. If it is a production environment, **ask before deploying** (rule 4).
3. Pick the operation that matches the change:
   - **`krill_deploy`** for a normal deploy. An `image` app takes `tag` to move to a new tag; a `dockerfile` app is cloned and built with layer cache.
   - **`krill_rebuild`** when the build must not reuse cached layers (`dockerfile` apps only).
   - **`krill_reload`** when nothing was rebuilt and the process just needs a bounce.
4. `krill_deploy` and `krill_rebuild` are asynchronous: they return a `deployment_id` right away and the build runs on the server. Poll `krill_deployment_status` with that id, on a widening interval (rule 1).
5. On `done`, verify with `krill_app_status` that replicas read `n/n` and the status is `running`. A green deployment with `0/1` replicas means the container started and died — go to the diagnose branch.
6. On `error`, read the `log_tail` the same call returned, name the cause, and stop (rule 2).

`krill_set_env` writes the variable but does **not** restart anything: the app keeps running the old value until the next deploy. If the change is meant to take effect, deploy after it — and say that you are about to.

## When the image has to be built locally

`krill_deploy` moves an `image` app to a tag that **already exists in a registry**. It cannot build one: the image bytes live on the user's machine, and no MCP tool can carry them.

So when the user's change is source code that has not been built and pushed yet, the deploy tool is not the answer — `krill-cli` is. If it is installed and you are working in the application's own repository:

1. Check there is a `krill.yaml` (`krill-cli` needs one; `krill-cli init` writes it).
2. Run `krill-cli deploy` through the shell. It builds locally, pushes, deploys and waits, and it verifies the token, the app and the image repository *before* the build.
3. Read its exit code rather than its prose: `0` ok · `1` the deployment failed · `2` configuration, arguments or a missing app · `3` another deploy was in flight (only with `--no-wait-for-lock`; by default it waits) · `4` timed out watching, **or the outcome could not be read** · `5` deployed but not running · `6` authentication. Codes `4` and `5` both mean "do not assume this worked" — check with `krill_app_status` before reporting anything.
4. Verify with `krill_app_status` as usual.

Two things this branch does not change. **Rule 4 still applies** — building and shipping arbitrary local code to production is a bigger action than moving a tag, not a smaller one, so ask first. And if `krill-cli` is not installed, or the shell is not in the application's repository, say so and stop; do not hand-roll `docker build` and `docker push` to work around it.

## Diagnose a failure

1. `krill_app_status` — is it `running`, `error`, `idle`? What do replicas say, and when was the last deployment?
2. `krill_deployments` — did the failure start with a deploy? Line the timestamps up against when the user says it broke. A service that has been down since a deploy and one that died on its own point at different causes.
3. `krill_deployment_status` on the suspect deployment — the build log tail usually names a failed build outright.
4. `krill_app_logs` with `level: "error"` first; if that is empty or unhelpful, widen to `info` and raise `tail`. An app with unstructured logs has no detectable levels, so the filter keeps all of its lines — read them rather than assuming the result is all errors. Empty logs at `0/n` replicas usually mean the container never got far enough to log.
5. `krill_list_env` when the logs point at configuration — a missing variable is a finding worth reporting (rule 3).

Finish with a hypothesis and the evidence for it: which deployment, which log line, what you think happened. Propose the fix; do not silently apply one.

## Four rules

**1. Poll on a widening interval.** First check ~5s after enqueueing, then roughly every 10s, and stop at about ten minutes — report where it got to and hand back. A build takes minutes; polling every second fills your own context with a hundred identical "running" answers and evicts the task you were called for. It also burns the per-token budget of 60 requests/minute, which the user's other calls share.

**2. Do not retry a failed deploy.** Nothing changed between the two attempts, so the same code fails the same way, and the second identical failure buries the first one's cause under a fresh log. Pull the log tail, name the cause, stop. Retry only after the user has changed something — and then say what changed.

**3. Never invent an environment variable's value.** The tools return names, not values, deliberately: everything you read reaches the model provider. You cannot see what `DATABASE_PASSWORD` is, so you cannot restore it, and a plausible-looking value written over a real one breaks the app quietly. Report a missing or suspect variable; set one only with a value the user gave you.

**4. Production deploys ask first.** One explicit confirmation before deploying, rebuilding, stopping or reloading anything in a production environment. Separately: a read-level token cannot write at all, and rights are re-resolved on every call from the token's level and its owner's current role — so check `krill_whoami` (`can_write`) before a write instead of discovering it from a rejection.

## Errors

Error messages are written to be acted on — "this is a dockerfile app, use rebuild" rather than "invalid argument". Read them before changing approach. An app in another organization reports as *not found*, not *forbidden*, so a not-found on an app the user insists exists means the token is scoped to a different org: check `krill_whoami`.
