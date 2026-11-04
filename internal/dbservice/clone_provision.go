package dbservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// CloneDatabase is the journaled identity of one NEW logical database. Deadline
// is immutable across execution and cleanup: a delayed executor must never gain
// a fresh authorization window. Password is transient and never journaled.
type CloneDatabase struct {
	Operation string
	DBName    string
	Username  string
	Deadline  time.Time
}

var cloneOperationPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var clonePasswordPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{20,128}$`)

func (c CloneDatabase) validate() error {
	if !cloneOperationPattern.MatchString(c.Operation) || !ValidIdent(c.DBName) || !ValidIdent(c.Username) || c.Deadline.IsZero() {
		return errors.New("invalid clone database identity")
	}
	return nil
}

func (c CloneDatabase) key() string    { return c.Operation + ":" + c.DBName + ":" + c.Username }
func (c CloneDatabase) marker() string { return "krill.environment-clone.v1:" + c.key() }
func (c CloneDatabase) expiry() string { return c.Deadline.UTC().Format(time.RFC3339Nano) }

// The fence lives in the instance's postgres database, never inside a tenant DB.
// This is bounded metadata, not roles or jobs. A resource's immutable expiry is
// checked UNDER its lock before inserting or checking a fence. Thus deleting
// expired tombstones cannot authorize a paused, old provisioning executor.
// Ownership checks fail closed if the reserved schema/table belong to anyone
// else; no DROP/ALTER of unrecognized metadata is attempted.
const cloneFenceSetup = `
\set ON_ERROR_STOP on
SET statement_timeout = '20s';
SET lock_timeout = '10s';
SET idle_in_transaction_session_timeout = '20s';
SET idle_session_timeout = '20s';
SET search_path = pg_catalog;
BEGIN;
SELECT pg_advisory_xact_lock(hashtextextended('krill.environment-clone.fence-schema.v1', 0));
DO $krill$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'krill_environment_clone') THEN
    CREATE SCHEMA krill_environment_clone;
    COMMENT ON SCHEMA krill_environment_clone IS 'krill.environment-clone.fences.v1';
    REVOKE ALL ON SCHEMA krill_environment_clone FROM PUBLIC;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'krill_environment_clone'
    AND nspowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
    AND obj_description(oid, 'pg_namespace') = 'krill.environment-clone.fences.v1') THEN
    RAISE EXCEPTION 'clone fence namespace ownership mismatch';
  END IF;
  IF to_regclass('krill_environment_clone.fences') IS NULL THEN
    CREATE TABLE krill_environment_clone.fences (
      operation text PRIMARY KEY, expires_at timestamptz NOT NULL, aborted boolean NOT NULL
    );
    COMMENT ON TABLE krill_environment_clone.fences IS 'krill.environment-clone.fences.v1';
    REVOKE ALL ON krill_environment_clone.fences FROM PUBLIC;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_class WHERE oid = 'krill_environment_clone.fences'::regclass
    AND relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
    AND obj_description(oid, 'pg_class') = 'krill.environment-clone.fences.v1') THEN
    RAISE EXCEPTION 'clone fence table ownership mismatch';
  END IF;
END $krill$;
COMMIT;
`

// A single permanent watermark row makes tombstone pruning safe even if the
// server clock later moves backwards. Empty operation is not a valid resource
// key. Never remove this row or decrease its cutoff.
const cloneFencePrune = `
INSERT INTO krill_environment_clone.fences VALUES ('', clock_timestamp(), true)
ON CONFLICT (operation) DO UPDATE
SET expires_at = greatest(krill_environment_clone.fences.expires_at, EXCLUDED.expires_at);
DELETE FROM krill_environment_clone.fences
WHERE operation <> '' AND expires_at <=
  (SELECT expires_at FROM krill_environment_clone.fences WHERE operation = '');
`

func (c CloneDatabase) lockSQL() string {
	return fmt.Sprintf("SELECT pg_advisory_lock(hashtextextended('krill.environment-clone:%s', 0));\n", c.key())
}

func (c CloneDatabase) provisionSQL(password string) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	// Generated passwords are hex/base64url. Do not allow SQL syntax or control
	// characters even if a future caller accidentally supplies another format.
	if !clonePasswordPattern.MatchString(password) {
		return "", errors.New("invalid generated clone password")
	}
	return cloneFenceSetup + c.lockSQL() + fmt.Sprintf(`
DO $krill$
BEGIN
  IF clock_timestamp() >= '%s'::timestamptz OR EXISTS (
    SELECT 1 FROM krill_environment_clone.fences WHERE operation = '' AND expires_at >= '%s'::timestamptz
  ) THEN
    RAISE EXCEPTION 'clone authorization expired';
  END IF;
  INSERT INTO krill_environment_clone.fences VALUES ('%s', '%s', false)
    ON CONFLICT (operation) DO NOTHING;
  IF NOT EXISTS (SELECT 1 FROM krill_environment_clone.fences
    WHERE operation = '%s' AND NOT aborted AND expires_at = '%s'::timestamptz) THEN
    RAISE EXCEPTION 'clone authorization revoked';
  END IF;
END $krill$;
BEGIN;
CREATE USER %q PASSWORD '%s';
COMMENT ON ROLE %q IS '%s';
COMMIT;
CREATE DATABASE %q OWNER %q TEMPLATE template0;
REVOKE CONNECT ON DATABASE %q FROM PUBLIC;
`, c.expiry(), c.expiry(), c.key(), c.expiry(), c.key(), c.expiry(), c.Username, password,
		c.Username, c.marker(), c.DBName, c.Username, c.DBName), nil
}

func (c CloneDatabase) cleanupSQL() (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	return cloneFenceSetup + c.lockSQL() + fmt.Sprintf(`
INSERT INTO krill_environment_clone.fences VALUES ('%s', '%s', true)
  ON CONFLICT (operation) DO UPDATE SET aborted = true;
SELECT format('DROP DATABASE %%I WITH (FORCE)', d.datname)
FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba
WHERE d.datname = '%s' AND r.rolname = '%s'
  AND shobj_description(r.oid, 'pg_authid') = '%s'
\gexec
SELECT format('DROP ROLE %%I', r.rolname) FROM pg_roles r
WHERE r.rolname = '%s' AND shobj_description(r.oid, 'pg_authid') = '%s'
\gexec
`, c.key(), c.expiry(), c.DBName, c.Username, c.marker(), c.Username, c.marker()) + cloneFencePrune, nil
}

// ProvisionCloneDatabase requires the caller to hold the instance lifecycle lock
// and persist identity/deadline before calling. It NEVER compensates itself:
// only the durable coordinator can distinguish failed from published work.
func (s *Service) ProvisionCloneDatabase(ctx context.Context, inst Instance, c CloneDatabase, password string) error {
	script, err := c.provisionSQL(password)
	if err != nil {
		return err
	}
	return s.cloneExec(ctx, inst, script, "provision")
}

// CleanupCloneDatabase revokes even a provisioning executor paused before its
// first lock. Repeating cleanup is safe, including after its reply was lost.
func (s *Service) CleanupCloneDatabase(ctx context.Context, inst Instance, c CloneDatabase) error {
	script, err := c.cleanupSQL()
	if err != nil {
		return err
	}
	return s.cloneExec(ctx, inst, script, "cleanup")
}

// PruneCloneFences removes expired metadata for both successful and aborted
// operations. The coordinator schedules it for every touched instance after the
// immutable deadline, retaining that intent until success (including outages).
// No role is used as a tombstone; only this one owned table is retained.
func (s *Service) PruneCloneFences(ctx context.Context, inst Instance, through time.Time) error {
	script, err := cloneFencePruneSQL(through)
	if err != nil {
		return err
	}
	return s.cloneExec(ctx, inst, script, "fence pruning")
}

func cloneFencePruneSQL(through time.Time) (string, error) {
	if through.IsZero() {
		return "", errors.New("missing clone fence cutoff")
	}
	// The panel only requests this after the immutable deadline. Advance to
	// that cutoff even when the target PG clock lags the panel, or marking the
	// journal pruned could strand still-unexpired metadata forever.
	cutoff := fmt.Sprintf("greatest(clock_timestamp(), '%s'::timestamptz)", through.UTC().Format(time.RFC3339Nano))
	script := strings.Replace(cloneFencePrune, "clock_timestamp()", cutoff, 1)
	return cloneFenceSetup + script, nil
}

func (s *Service) cloneExec(ctx context.Context, inst Instance, script, stage string) error {
	if s.engine == nil {
		return errors.New("clone database engine unavailable")
	}
	// psql -X prevents a user psqlrc from logging SQL/passwords or overriding
	// our session contract. Engine.Exec errors contain stderr (possibly SQL),
	// so do not wrap or log them. Surface only safe context and cancellation.
	err := s.engine.Exec(ctx, inst.AppName,
		[]string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", inst.Superuser, "-d", "postgres"},
		[]string{"PGPASSWORD=" + inst.SuperuserPassword}, strings.NewReader(script), io.Discard)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("clone database %s: %w", stage, ctx.Err())
		}
		return fmt.Errorf("clone database %s failed", stage)
	}
	return nil
}
