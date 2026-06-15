-- WARNING: this drops all app_volumes/volume_backups rows. Docker volumes on
-- disk are NOT removed and become orphaned (nothing left to enumerate them).
-- Before reversing in prod, record volume names (`docker volume ls | grep krill-vol-`)
-- for manual cleanup.
DROP TABLE IF EXISTS volume_backups;
DROP TABLE IF EXISTS app_volumes;
