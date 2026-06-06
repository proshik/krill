-- WARNING: destructive — drops the core tables (applications, sessions, users) and ALL their data.
-- Down migrations are for local/dev rollback only; Krill applies migrations up-only on startup.
DROP TABLE IF EXISTS applications;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
