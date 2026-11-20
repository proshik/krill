# Backups of Krill's own state

Krill keeps everything it knows (organizations, projects, apps, domains, settings, deploy
history, encrypted credentials) in its control-plane PostgreSQL database. **Settings → Krill
backups** dumps that database on a schedule, encrypts the dump with your public key and uploads
it to S3-compatible storage. A snapshot is also taken automatically before every update from the
UI. The page is visible to instance operators only.

This is separate from [Backups](backups.md), which covers your apps' databases and volumes.
What a state backup restores is Krill's configuration, not your workloads: see
[Restore Krill's state from a backup](../install.md#restore-krills-state-from-a-backup).

## 1. Generate the key pair

Backups are encrypted with [age](https://age-encryption.org). Generate the key on your own
machine, never on the Krill server:

```sh
age-keygen -o krill-backup-key.txt
```

The command prints `Public key: age1…`. Paste that value into the page. Keep
`krill-backup-key.txt` (the private key) somewhere other than the Krill server, together with
your `KRILL_SECRET_KEY`. Krill never stores either of them, and the backups do not contain them:

- Without the private key a backup cannot be read.
- Without `KRILL_SECRET_KEY` a restore brings back everything except the encrypted secrets
  (database passwords, credentials, SSH keys, bot tokens).

Only X25519 recipients (`age1…`) are accepted. If `KRILL_SECRET_KEY` is empty, stored secrets
are plain text in the database and so inside every backup (still age-encrypted).

## 2. Set up storage

Enter the endpoint, bucket, region, access key and secret key, and a prefix. Use a **separate
bucket or prefix for each Krill installation**: retention deletes old objects under the prefix,
and two installations sharing one would prune each other's backups. Objects land under
`<prefix>/krill-state/` (scheduled) and `<prefix>/krill-state/pre-update/` (before updates).

Saving checks the storage with a `HeadBucket` call and refuses to save if it fails, so a wrong
bucket shows up now rather than at the first scheduled run. An empty secret-key field keeps the
stored key only while the endpoint keeps the same scheme, host and port; otherwise Krill asks for
the key again so a stored credential never goes to another host.

All S3 traffic goes through the same egress guard as other storage. A private endpoint (an
address on your own network) needs `KRILL_ALLOW_PRIVATE_EGRESS=true`; see
[Configuration](../configuration.md).

The credentials need these S3 permissions on the bucket and the prefix:

- `s3:ListBucket` and `s3:GetBucketLocation` (the save check is a `HeadBucket` call, and
  retention lists the prefix);
- `s3:PutObject` and `s3:AbortMultipartUpload` (the upload is multipart);
- `s3:GetObject` (after an upload Krill reads the object's size back with `HeadObject`, which
  AWS authorizes with `GetObject`);
- `s3:DeleteObject` (retention).

The save check covers only the bucket, not the objects, so a key that lacks the rest passes it
and fails at the first real backup. That could be the snapshot before an update, which then
refuses to start. **Press Back up now right after saving and check that the run in "Recent
backups" says OK**; this proves the key, the endpoint and the permissions in one step.

Add a lifecycle rule to the bucket that aborts incomplete multipart uploads after one day. If
Krill is killed in the middle of a backup, the unfinished upload stays in the bucket, invisible
and billed, until such a rule removes it.

## 3. Schedule, pause and on demand

Pick a schedule preset: hourly, daily, weekly, monthly or a custom cron expression. **Off**
means on demand only: nothing runs on a timer, but **Back up now** and the pre-update snapshot
still work.

**Saving does not turn the automatic backups on.** On a new installation the page stays
**Paused** after the first save (storage and key set, schedule chosen): **Back up now** works,
but no scheduled backup runs and no snapshot is taken before an update. Press **Resume**. The
status at the top of the page then reads **Active** (with the time of the next backup), or
**On demand only** when the schedule is Off. If it still says **Paused**, nothing automatic
happens, and the Updates page says so too ("this update will not be preceded by a backup").

**Pause** stops the scheduled backups and the pre-update snapshot. **Back up now** still works
while paused, as long as storage and the age key are set. "Keep last N backups" applies to the
scheduled directory; the `pre-update/` directory always keeps the 5 newest.

## 4. Before every update

When you install a release from **Settings → Updates** and the backups are enabled (not
paused, with storage and the key set), Krill first takes a snapshot named
`…_<from>_to_<to>.sql.gz.age` under `pre-update/`. If it fails, the update does not start and
the page shows why: an update that cannot be undone from a backup is not worth the risk.

To update anyway (for instance, while the storage is down), pause the backups, run the update,
and resume them afterwards. If backups are not set up at all, updates proceed as before.

While an update is running, **Back up now** is refused and a scheduled backup is skipped: the
restart at the end of the update would cut it short, and the update's own snapshot covers that
moment.

## 5. What is in a backup

The dump is a plain `pg_dump` of the Krill database, compressed and encrypted as it streams.
Nothing unencrypted is written to disk or to the bucket. The **data** of two tables is left out
because it has no value after a restore: `sessions` (live login tokens, which would otherwise be
valid sign-ins inside every backup) and `metric_samples` (monitoring history, large and
re-created every 30 seconds). Their structure is kept.

For the bundled database the dump runs in a short-lived container on the host network using the
same `postgres:<major>-alpine` image major as the server. For an external database the same
container connects with your `KRILL_DATABASE_URL`. The database password is handed to that
container on its standard input, never in its environment or command line, so it does not show
up in `docker inspect` or in a debug-level Docker daemon log; a password containing a line break
is refused. A backup is kept only when the dump's output ends with a completion line that the
container prints, with a value unique to that run, after `pg_dump` succeeded: output cut short
anywhere fails the run instead of storing a partial dump. A `verify-full` or `verify-ca` connection
without `sslrootcert` uses the host's CA bundle, and so does `sslrootcert=system` (with
`verify-full`, since the `pg_dump` of PostgreSQL 13–15 does not know `system`). When Krill's
environment sets `SSL_CERT_FILE`, that file is the CA bundle instead, as it is for Krill's own
connection; `SSL_CERT_DIR` cannot be reproduced in the container, so with it the backup is
refused until the URL names `sslrootcert=<path>`. Relative
certificate paths and unix-socket connections are refused. TLS settings Krill itself takes from its environment (`PGSSLMODE`,
`PGSSLROOTCERT`, `PGSSLCERT`, `PGSSLKEY`, `PGSSLNEGOTIATION`, `PGCHANNELBINDING`) when the URL
leaves them out reach the dump the same way, and so do `sslnegotiation` and `channel_binding`
in the URL. `sslnegotiation=direct` needs the `pg_dump` of PostgreSQL 17 or newer, so with an
older server the backup is refused; it also turns the default `sslmode` into `require`, as it
does for Krill's own connection, and is refused with `sslmode=allow`. A connection that relies on a PostgreSQL service file (`service=` or `PGSERVICE`) is
refused, because the container cannot read its settings: put them in `KRILL_DATABASE_URL`.
A URL that gives a connection or TLS parameter (`sslmode`, `sslrootcert`, `channel_binding`,
…) more than once is refused too: Krill's own connection and `pg_dump` would not agree on which
value applies.

## 6. Key rotation

Changing the public key affects new backups only. Keep the old private key until the older
backups have rotated out of retention (and the `pre-update/` directory has passed them too).

## 7. Alerts and the digest

A failed backup sends a Telegram alert, but only to channels that have node and infrastructure
messages enabled (see [Notifications](notifications.md)), because backup failures concern the
instance, not one organization. The daily digest of such a channel gets a **Krill state** line:
the last backup and how many exist; a yellow line when backups are not set up, are paused, or
when a scheduled backup is overdue (the cron schedule is evaluated in the host's local time
zone, with one hour of grace); and a red line when the last backup failed. On-demand mode shows
a neutral line.

## 8. Restoring

Restores are done by hand, following the runbook in
[Restore Krill's state from a backup](../install.md#restore-krills-state-from-a-backup). It
also describes a regular restore test, which proves both the object and your private key.
