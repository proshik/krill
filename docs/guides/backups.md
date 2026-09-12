# Backups

Krill backs up **Postgres databases** and **app volumes** to S3-compatible storage — AWS S3,
MinIO, or any other service that speaks the S3 API. Both use the same form and the same
scheduling.

Not covered: Redis, DragonFly and MinIO instances, and **Krill's own state database**. Back
that one up yourself, or install Krill against a managed Postgres — see
[Installing Krill](../install.md#use-a-managed--external-postgres).

## 1. Add storage

Open **Settings → Storage** and add a bucket: a name, the endpoint (blank for AWS, a URL for
MinIO and others), bucket, region, and access keys. The backup form also offers this inline
when the organization has no storage yet.

The keys are encrypted at rest when `KRILL_SECRET_KEY` is set, as it is on every install made
by the installer. Storage on a private address (a MinIO on your LAN) needs
`KRILL_ALLOW_PRIVATE_EGRESS=true`.

## 2. Create a backup

On a database's page — or an app's **Volumes** tab — open **Backups** and add one:

- **Storage** — where the files go.
- **Schedule** — On-demand, Hourly, Daily, Weekly, Monthly, or Custom. A custom cron
  expression and an S3 key prefix are under **Advanced**.
- **Keep last N backups** — older files are deleted after each run.

## 3. Run and restore

- Backups run on their schedule, or at any time with **Backup now**. **Pause** stops the
  schedule without deleting anything.
- A database backup is `pg_dump --clean --if-exists`, run inside the database's container and
  streamed gzipped to `s3://<bucket>/<prefix>/<instance>/<database>/<timestamp>.sql.gz`. A
  volume backup is a tar archive of the volume made by a short-lived sidecar container.
- **Restore** streams a file back through `psql`, or unpacks it into the volume. It
  **replaces** the current contents — files created after the backup are removed — so it asks
  for confirmation, and it stops the app before restoring a volume. The database must be
  running.
- **Download** streams a backup file through Krill.
