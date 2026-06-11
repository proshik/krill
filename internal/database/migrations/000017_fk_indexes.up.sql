-- Indexes on the FK columns the org dashboard and per-env listings filter by.
-- Without them, ListProjectsWithCounts' joins and the per-environment app/db
-- lookups are sequential scans.
CREATE INDEX IF NOT EXISTS environments_project_id_idx ON environments (project_id);
CREATE INDEX IF NOT EXISTS applications_environment_id_idx ON applications (environment_id);
CREATE INDEX IF NOT EXISTS postgres_dbs_environment_id_idx ON postgres_dbs (environment_id);
CREATE INDEX IF NOT EXISTS redis_dbs_environment_id_idx ON redis_dbs (environment_id);
