-- Allow the transient 'migrating' status (volume migration job). The inline
-- column CHECK from 000029 is Postgres-auto-named db_instances_status_check;
-- recreate it rather than adding a second CHECK (same pattern as 000036).
ALTER TABLE db_instances DROP CONSTRAINT db_instances_status_check;
ALTER TABLE db_instances ADD CONSTRAINT db_instances_status_check
    CHECK (status IN ('idle', 'running', 'error', 'migrating'));
