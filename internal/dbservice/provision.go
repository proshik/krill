package dbservice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// identRe — allowed postgres identifiers we create (db names, usernames).
// Strict on purpose: combined with quoting below it makes DDL injection impossible.
var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func ValidIdent(s string) bool { return identRe.MatchString(s) }

// SanitizeIdent derives a valid identifier from a display name (lowercase,
// non-alnum → underscore, digit-leading and empty results get a db_ prefix).
func SanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "db"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "db_" + out
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// createDBSQL — the three provisioning statements, fed to psql via STDIN. Stdin
// (not -c) for two reasons: psql sends stdin statements one at a time (a multi-
// statement -c runs in one implicit transaction, and CREATE DATABASE refuses to
// run inside one), and the new user's password never appears in exec argv.
func createDBSQL(dbName, username, password string) string {
	return fmt.Sprintf("CREATE USER %q PASSWORD '%s';\nCREATE DATABASE %q OWNER %q;\nREVOKE CONNECT ON DATABASE %q FROM PUBLIC;\n",
		username, password, dbName, username, dbName)
}

func dropDBSQL(dbName, username string) string {
	return fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE);\nDROP USER IF EXISTS %q;\n", dbName, username)
}

// psqlExec runs statements as the instance superuser inside its container.
func (s *Service) psqlExec(ctx context.Context, inst Instance, sqlText string, out io.Writer) error {
	return s.engine.Exec(ctx, inst.AppName,
		[]string{"psql", "-v", "ON_ERROR_STOP=1", "-U", inst.Superuser, "-d", "postgres"},
		[]string{"PGPASSWORD=" + inst.SuperuserPassword},
		strings.NewReader(sqlText), out)
}

// InstanceRunning reports whether the instance's service has a running task.
// Engine-gated (nil engine → true) so nil-engine tests skip the liveness check,
// same convention as Engine.RegistryCheck.
func (s *Service) InstanceRunning(ctx context.Context, inst Instance) bool {
	if s.engine == nil {
		return true
	}
	st, err := s.engine.ServiceState(ctx, inst.AppName)
	return err == nil && st.Found && st.Running >= 1
}

// ProvisionLogicalDB creates the database + its owner user inside a running
// postgres instance. REVOKE CONNECT FROM PUBLIC isolates it from the instance's
// other users. On mid-way failure the created user is dropped (compensation),
// so a retry is safe. Engine-gated (nil engine → no-op, tests only).
func (s *Service) ProvisionLogicalDB(ctx context.Context, inst Instance, ldb LogicalDB) error {
	if s.engine == nil {
		return nil
	}
	if !ValidIdent(ldb.DBName) || !ValidIdent(ldb.Username) {
		return fmt.Errorf("invalid db or user identifier")
	}
	if strings.ContainsAny(ldb.Password, "'\\") {
		return fmt.Errorf("generated password contains forbidden characters")
	}
	var buf bytes.Buffer
	if err := s.psqlExec(ctx, inst, createDBSQL(ldb.DBName, ldb.Username, ldb.Password), &buf); err != nil {
		_ = s.psqlExec(ctx, inst, fmt.Sprintf("DROP USER IF EXISTS %q;\n", ldb.Username), io.Discard)
		return fmt.Errorf("provision database %s: %w: %s", ldb.DBName, err, strings.TrimSpace(buf.String()))
	}
	return nil
}

// DropLogicalDB drops the database (terminating live connections) and its owner
// user. Engine-gated (nil engine → no-op, tests only).
func (s *Service) DropLogicalDB(ctx context.Context, inst Instance, ldb LogicalDB) error {
	if s.engine == nil {
		return nil
	}
	if !ValidIdent(ldb.DBName) || !ValidIdent(ldb.Username) {
		return fmt.Errorf("invalid db or user identifier")
	}
	var buf bytes.Buffer
	if err := s.psqlExec(ctx, inst, dropDBSQL(ldb.DBName, ldb.Username), &buf); err != nil {
		return fmt.Errorf("drop database %s: %w: %s", ldb.DBName, err, strings.TrimSpace(buf.String()))
	}
	return nil
}
