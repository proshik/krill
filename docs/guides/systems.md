# Systems in an environment

An environment that runs several products side by side — say Penpot's five components, a
small web app and a bot — shows them as one group until you split them. A **system** is a
named group of an environment's apps. Admins manage systems on the environment's **Settings**
page, under **Systems**: add one by name, then expand a system's row to rename it, add apps (an
app is in at most one system, so adding it moves it out of any other), remove them, or delete
the system.

On the environment's canvas each system is a framed group with its apps on the left and the
resources those apps use on the right:

- logical Postgres databases (`PG penpot @ pg-main`),
- buckets (`S3 penpot @ silo`),
- whole servers linked directly, such as a Valkey or Redis cache.

Pointing at or tabbing to an app lights up the wires to the resources it uses; pointing at a
resource lights up the apps using it. A resource used outside this system, or one on a server
that also hosts databases or buckets the system does not use, is shared — the system never owns
it. A dashed wire is not a modeled link: Krill found the resource's address in the app's
environment values. Apps in no system are in the group **not in a system**.

A system owns nothing. Removing one makes its apps ungrouped; no app, deployment, link,
database, bucket or server changes. Members of the organization see the systems; owners and
admins manage them. Cloning an environment does not copy its systems.
