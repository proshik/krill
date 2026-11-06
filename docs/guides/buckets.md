# Buckets on a managed S3 server

A managed S3 server (**Database Servers → New**, engine *S3 (Silo)*) belongs to the
organization. A bucket on it belongs to one environment, the way a logical Postgres database
does, and comes with its own access key that can use that bucket and nothing else. Linked
apps get that key; the server's root credentials are never injected through a bucket link.

New S3 servers run [Silo](https://hub.docker.com/r/pgsty/silo), a maintained MinIO fork. Krill
manages buckets with the `mc` client inside the server's own container, so a server running a
custom image without `mc` can still serve S3 but cannot have buckets managed by Krill.

## Create or import a bucket

Open the environment, then **Buckets → New bucket** (organization admins and owners):

- **Create** makes the bucket, a policy granting every S3 action on that bucket only
  (`krill-bucket-<name>`), and a user `krill-<name>-<random>` holding that policy.
- **Import** adopts a bucket that already exists, with a key you already use for it. Nothing is
  recreated: Krill writes, reads and deletes one probe object with that key to prove it works,
  then records the bucket as *imported*.

The bucket page shows the endpoint, bucket and access key to everyone in the organization and
the secret key to admins only.

## Link it to an app

On the app's **Environment** tab, **Linked buckets** injects one value per variable on every
deploy: `endpoint` (the server's address on the organization network, `http://<server>:9000`),
`bucket`, `access_key`, `secret_key` or `region` (`us-east-1`). A variable already set in the
environment text, used by the metrics token or injected by another link is refused. The agent
API lists these variables with the source `bucket-link`, never their values.

Linking a whole S3 server through **Linked databases** is still possible and is marked *root
credentials*: it injects the server's root user, which can do anything on the server.

## Rotate a key

**Rotate key** issues a new user for the bucket and makes it the one links inject. The previous
key keeps working: redeploy the linked apps, check them, then **Revoke** the previous key. A
second rotation is refused until the previous key is revoked.

## Remove a bucket

A bucket that apps still link to cannot be removed, and an S3 server that still holds buckets
cannot be deleted. **Remove bucket** forgets the bucket in Krill and removes the users Krill
generated for it; the bucket and its objects stay on the server and can be imported again.
Tick **Also delete its data** to empty and remove the bucket. An imported key is never removed
by Krill except through **Revoke** after a rotation.

Deleting an environment removes the users Krill generated for its buckets but keeps the
buckets and their objects on the server. Cloning an environment does not copy buckets or
bucket links.

Buckets are not backed up yet; see [Backups](backups.md).
