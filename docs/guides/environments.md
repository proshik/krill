# Clone an environment

An organization admin or owner can open a project, select an environment and
choose **Clone environment**. Enter a new name: the suggested name is `staging`
for `production`, or `<source>-copy` otherwise. An existing name produces an
error; Krill does not silently rename the clone.

Cloning copies saved application configuration within the same project and
organization. It does not deploy applications or copy production data.

| Configuration | Result |
| --- | --- |
| Runtime, resources, healthchecks, command and predeploy command | Copied; commands run only on explicit Deploy |
| Ordinary/public environment values and build arguments | Copied literally, including comments and ordering |
| Secret environment values | Assignments retained with empty values |
| Build secrets | Cleared |
| Modeled database links | Remapped to new resources; overriding raw literals are cleared, including public ones |
| PostgreSQL | New empty logical databases and owner credentials on the existing running server |
| Buckets and bucket links | Not copied |
| Linked Redis, Valkey, DragonFly and MinIO servers | One new stopped configuration per distinct source server, with a fresh password |
| Domains | One new internal primary domain per app; no source/custom/shared hosts, routes, TLS or access credentials |
| Volumes | New empty named volumes, created at Deploy |
| Placement | Same selected nodes; the app's own labels are created at Deploy |
| Metrics | Endpoints and settings copied with a fresh token |
| Auto-deploy, webhooks, host ports, backups and history | Not copied |

Registry and Git credential references stay within the organization and must
match the source host. Git URL user-info is stripped; credential-bearing query
parameters are rejected. Ordinary Git URL components are preserved.

Before cloning, review environment variable classifications: ordinary values are
copied literally and secret assignments are emptied. Classification does not
change normal deployment injection for the source application.

Before Deploy, fill secret values and build secrets, review all copied public
URLs and commands, and configure exposure on the new domain if needed. Krill
cannot infer whether a literal public URL points to production and does not
rewrite it. A modeled database link supplies its remapped connection value at
every deployment. Its raw environment assignment is intentionally empty.

PostgreSQL servers must already be running and reachable. Krill never starts or
restarts a source server for cloning. Existing modeled logical databases on each
source PostgreSQL instance must deny CONNECT to PUBLIC. Legacy databases can
retain that default grant; ask the server administrator to review and revoke it
before cloning. Krill refuses unsafe cloning without modifying source ACLs.
It checks PUBLIC before provisioning and the new role's effective CONNECT access
before publication. CONNECT alone does not imply access to table contents;
administrators remain responsible for later ACL changes. PostgreSQL isolation is a new database and
role on the same instance, not separate compute capacity. Redis, DragonFly and
MinIO configurations need an explicit **Deploy** in **Database Servers**. These
remain organization resources after the cloned environment is deleted; delete
them separately when no longer needed. No external or console ports are copied.

## Interrupted operations

The target environment becomes visible only after all configuration is saved
atomically. Repeated submission of the same request returns its recorded target.
An interrupted clone is cleaned up, never automatically resumed. Confirmed cleanup
steps are recorded so later passes continue with the remaining resources. Cleanup checks
ownership before removing newly provisioned PostgreSQL resources; it never drops
source databases. A cancelled Docker exec is not treated as proof that its SQL
has stopped: a durable PostgreSQL abort fence prevents late provisioning.

If a server is unreachable, cleanup remains recorded and retries in bounded
background passes. This guarantees no partially published environment and no
untracked new external objects, not immediate cleanup during an outage. Pending
cleanup alone does not block self-update; executing clone/cleanup work does.
The journal contains resource identities, not passwords or source secret values.
If a server is deleted before pending cleanup succeeds, its journal remains but
operator intervention may be needed to reconnect the retained data volume.

The additive state migration permits binary rollback. Older binaries do not run
clone recovery; outstanding ownership records remain until a clone-capable
binary runs again. Keep the target server's `krill_environment_clone` metadata
schema intact: it carries abort fences and a bounded permanent pruning watermark.

Execution exclusion uses process-local named guards under Krill's single-control-plane model, not pooled SQL session leases. Durable reservations,
publication state and target PostgreSQL abort fences survive process restarts.
