-- Dev rollback only. A row caught mid-migration becomes 'error' so the
-- restored constraint holds.
UPDATE db_instances SET status = 'error' WHERE status = 'migrating';
ALTER TABLE db_instances DROP CONSTRAINT db_instances_status_check;
ALTER TABLE db_instances ADD CONSTRAINT db_instances_status_check
    CHECK (status IN ('idle', 'running', 'error'));
