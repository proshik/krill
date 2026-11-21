-- name: GetStateBackupSettings :one
SELECT * FROM state_backup_settings WHERE id = 1;

-- name: UpsertStateBackupSettings :exec
-- enabled is left out on purpose: a new row starts paused (the column's
-- default) and only SetStateBackupEnabled changes it, so a save that read
-- the flag before a concurrent pause or resume cannot undo it.
INSERT INTO state_backup_settings (id, endpoint, bucket, region, access_key, secret_key, prefix, recipient, schedule, retention, updated_at)
VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (id) DO UPDATE SET
    endpoint   = EXCLUDED.endpoint,
    bucket     = EXCLUDED.bucket,
    region     = EXCLUDED.region,
    access_key = EXCLUDED.access_key,
    secret_key = EXCLUDED.secret_key,
    prefix     = EXCLUDED.prefix,
    recipient  = EXCLUDED.recipient,
    schedule   = EXCLUDED.schedule,
    retention  = EXCLUDED.retention,
    updated_at = now();

-- name: SetStateBackupEnabled :execrows
UPDATE state_backup_settings SET enabled = $1, updated_at = now() WHERE id = 1;

-- name: InsertStateBackupRun :exec
INSERT INTO state_backup_runs (trigger, status, object_key, size_bytes, from_version, to_version, error, started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListStateBackupRuns :many
SELECT * FROM state_backup_runs ORDER BY finished_at DESC, id DESC LIMIT $1;

-- name: ListStateBackupRunsSince :many
SELECT * FROM state_backup_runs WHERE finished_at >= $1 ORDER BY finished_at, id;

-- name: LastSuccessfulStateBackupRun :one
SELECT * FROM state_backup_runs WHERE status = 'ok' ORDER BY finished_at DESC, id DESC LIMIT 1;

-- name: PruneStateBackupRuns :exec
-- The newest successful run is kept however old it is: the settings page and
-- the digest read the last success from it, and a monthly schedule's last run
-- would otherwise drop out of the journal before the next one is due.
DELETE FROM state_backup_runs AS r
WHERE r.finished_at < $1
  AND r.id IS DISTINCT FROM (
    SELECT l.id FROM state_backup_runs AS l WHERE l.status = 'ok' ORDER BY l.finished_at DESC, l.id DESC LIMIT 1
  );
