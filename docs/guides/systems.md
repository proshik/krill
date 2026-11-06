# Systems in an environment

An environment that runs several products side by side — say Penpot's five components, a
small web app and a bot — shows them as one flat list until you group them. A **system** is a
named group of an environment's apps: **+ System** on the environment page creates one, and
**Manage system** on its card renames it, adds apps (an app is in at most one system, so adding
it moves it out of any other) or removes them.

Each system card shows its apps with their live status, a count of running and failing ones,
and the resources those apps use:

- logical Postgres databases (`PG penpot @ pg-main`),
- buckets (`S3 penpot @ silo`),
- whole servers linked directly, such as a Valkey or Redis cache.

Pointing at or tabbing to a component highlights the resources it uses; pointing at a resource
highlights the components using it. A resource marked **shared** is used outside this system,
or lives on a server that also hosts databases or buckets the system does not use — the system
never owns it. A resource marked **detected** is not a modeled link: Krill found its address in
the app's environment values. Apps in no system are listed under **Ungrouped**; an environment
with no systems looks exactly as before.

A system owns nothing. Removing one makes its apps ungrouped; no app, deployment, link,
database, bucket or server changes. Members of the organization see the systems; owners and
admins manage them. Cloning an environment does not copy its systems.
