# Deployments

## Run a command before deployment

Organization admins can set **Advanced → Pre-deploy command**, for example
`./migrate up`. Leave it empty to disable it. Save the form, then deploy.

Krill runs the command once in a disposable container using the **new image**,
the application's resolved environment and its current organization network.
The image must contain `/bin/sh`; Krill overrides its entrypoint with
`/bin/sh -c`, preserving its user and working directory. The container receives
the application's effective CPU/memory limits, no application volumes and no
published ports. Image healthchecks and automatic restarts are disabled.

The old application stays running throughout this step. Exit status zero allows
Krill to prepare volumes and start the rolling update. A nonzero exit, timeout,
or cleanup failure fails the deployment before changing the application service.
The command has a five-minute limit, also bounded by the deployment's overall
ten-minute deadline. Shutdown cancels it and attempts to remove the container.
If a worker becomes unreachable, removal cannot be guaranteed; cleanup errors
are logged and startup tries to remove interrupted pre-deploy containers.
Commands are never automatically resumed.

For pinned/global placement, Krill chooses one ready, active selected node
(stable node-ID ordering), even if the application has several replicas. For
unrestricted placement it prefers an active manager, then another ready node.
A locally built image must exist on the selected node; Krill does not distribute
local images to workers. Registry images are pinned by digest before the command
runs; inability to resolve the digest fails this deployment instead of falling
back to a mutable tag. The service uses the same immutable image reference.

Deploy, Rebuild, deploy webhooks and API-triggered deployments share this step.
Reload of a running service does not deploy a new image and does not run it.
The internal organization-network migration skips it.

Database changes are **not undone** when the command or subsequent rolling
update fails. Write migrations compatible with the still-running old application
and safe to run again on a later deployment. Rollback of application code cannot
undo database migrations.

Command output appears in deployment logs. Krill masks literal application env
values, including values split across output chunks, before storing or streaming
the output. This cannot recognize values transformed or encoded by your command.
Do not print secrets or put them in the command text; use environment variables.

## Roll back an application release

In **Deployments**, organization admins can choose **Rollback** on a confirmed
release. The confirmation explains that this restores both the running release
and its runtime editor settings. Rollback is a fresh deployment, with a reference
to the original history entry. The same per-app and per-organization queue limits
apply. It skips building and the pre-deploy command, then waits for the new tasks
to become healthy and for Swarm's update to settle.

The snapshot restores the immutable image, source image/tag or Git coordinates,
raw environment text and its public/secret classification, container command,
internal HTTP port, replicas, effective CPU/memory limits, restart policy,
healthcheck and pre-deploy command. Runtime editors are updated only after a
successful rollout. A later normal **Deploy** reads these restored settings and
runs the restored pre-deploy command; it resolves the configured image tag or
Git branch again, so a mutable tag or branch may have advanced since the snapshot.
The snapshot keeps raw resource fields separately from effective limits. Rollback
uses the historical effective limits; a later normal Deploy applies current
instance defaults wherever the restored editor fields inherit them.

Network, domains, TLS, access protection and observability identity stay current.
Volumes, published ports and placement must still match the release; their
underlying volumes, images and selected nodes must remain available. Krill does
not recreate missing volumes or move the release to another node to make a
rollback succeed. Before starting the old image, rollback applies the configured
volume ownership, just like an ordinary deploy; it does not restore volume data.
Database-link identity and injected credentials, and metrics
configuration/token, must match the snapshot. Changes cause a refusal instead of
silently substituting a different credential. Literal connection strings in env
are restored as literals; Krill cannot verify external database state.

Database/volume contents, registry/Git credentials and build settings are not
restored. Git URL user-info is omitted from source provenance. Current registry
credentials must be usable for the old image's registry. A locally built image
must be present on every eligible placement node; registry images can be pulled
by their stored digest. Old deployment tags are retained, but no image retention
or distribution service is added: registry deletion or manual image pruning can
make a release unavailable. Krill never rebuilds or falls back to the current
contents of an old mutable tag during rollback.

Snapshots require **KRILL_SECRET_KEY** and are encrypted with record-bound AEAD.
Without a key, ordinary deployments still work, but no rollback snapshot is
created. Missing/wrong keys, damaged snapshots and unsupported snapshot versions
fail closed. Old history entries without a snapshot, failed deployments and
updates that never reached confirmed convergence have no rollback action.

Do not edit runtime settings while rollback is queued or rolling out. Krill checks
for changes before the service update and conditionally restores editor settings
after convergence. A concurrent edit is preserved and the rollback is recorded as
failed. If that edit arrived during rollout, the service may already be running
the selected older image while editors retain the concurrent edit; inspect the
history before deploying again. External topology changes can likewise race a
Docker operation, so dependency checks cannot provide a transaction over Docker,
PostgreSQL and external databases.
