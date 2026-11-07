package dbservice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"
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

// provisionCompensateTimeout bounds the compensating drop after a failed
// provisioning. Short: it is a couple of DDL statements against a container
// that was reachable moments ago.
const provisionCompensateTimeout = 30 * time.Second

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
// other users. On mid-way failure a full drop (database then user, both IF
// EXISTS) compensates whichever objects got created, so a retry is safe.
// Engine-gated (nil engine → no-op, tests only).
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
		// Compensation must cover all three DDL statements, not just CREATE USER:
		// a failure at REVOKE leaves both the user AND the database created, with
		// the user owning the database, so DROP USER alone fails (Postgres refuses
		// to drop a role that owns a database) and a retry would then fail forever
		// at CREATE USER. dropDBSQL runs DROP DATABASE before DROP USER (both IF
		// EXISTS, so it's still a no-op for failure points where objects were never
		// created), which is safe to compensate every failure point above.
		// Detached: the failure is often the context itself dying (an aborted
		// request, a timeout), and compensation riding that same context could
		// not run at all — leaving exactly the half-created state described
		// above, which no retry can recover from.
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), provisionCompensateTimeout)
		defer ccancel()
		if cerr := s.psqlExec(cctx, inst, dropDBSQL(ldb.DBName, ldb.Username), io.Discard); cerr != nil {
			slog.Error("logical db provisioning compensation failed — a half-created database/user may remain and block retries",
				"db_name", ldb.DBName, "user", ldb.Username, "err", cerr)
		}
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

// ConnStats is a read-only view of a Postgres server's connection budget and
// of what one logical database holds of it. Krill never terminates sessions:
// this only shows what to look at.
type ConnStats struct {
	Max           int // max_connections
	Total         int // client backends on the server
	Database      int // ... connected to this database
	Role          int // ... logged in as this database's owner role
	IdleInTx      int // ... of this database idle in a transaction
	OldestIdleTxS int // seconds the oldest of those has been idle in it
	AdvisoryLocks int // advisory locks held by sessions of this database
}

// connStatsSQL reads ConnStats in one row. dbName and user are validated
// identifiers (ValidIdent), so quoting them as literals is safe.
func connStatsSQL(dbName, user string) string {
	return fmt.Sprintf(`SELECT current_setting('max_connections')::int,
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'),
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND datname = '%[1]s'),
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND usename = '%[2]s'),
 (SELECT count(*) FROM pg_stat_activity WHERE datname = '%[1]s' AND state = 'idle in transaction'),
 (SELECT coalesce(max(extract(epoch FROM now() - state_change))::int, 0) FROM pg_stat_activity WHERE datname = '%[1]s' AND state = 'idle in transaction'),
 (SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE l.locktype = 'advisory' AND a.datname = '%[1]s');
`, dbName, user)
}

// ConnectionStats reads the connection budget of the instance and the share
// of it one logical database uses. Engine-gated (nil engine → zero, tests).
func (s *Service) ConnectionStats(ctx context.Context, inst Instance, ldb LogicalDB) (ConnStats, error) {
	if s.engine == nil {
		return ConnStats{}, nil
	}
	if !ValidIdent(ldb.DBName) || !ValidIdent(ldb.Username) {
		return ConnStats{}, fmt.Errorf("invalid db or user identifier")
	}
	var out bytes.Buffer
	err := s.engine.Exec(ctx, inst.AppName,
		[]string{"psql", "-At", "-F", " ", "-v", "ON_ERROR_STOP=1", "-U", inst.Superuser, "-d", "postgres"},
		[]string{"PGPASSWORD=" + inst.SuperuserPassword},
		strings.NewReader(connStatsSQL(ldb.DBName, ldb.Username)), &out)
	if err != nil {
		return ConnStats{}, fmt.Errorf("read connection stats: %w", err)
	}
	return parseConnStats(out.String())
}

func parseConnStats(out string) (ConnStats, error) {
	var c ConnStats
	line := strings.TrimSpace(out)
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = line[i+1:]
	}
	if _, err := fmt.Sscan(line, &c.Max, &c.Total, &c.Database, &c.Role, &c.IdleInTx, &c.OldestIdleTxS, &c.AdvisoryLocks); err != nil {
		return ConnStats{}, fmt.Errorf("parse connection stats %q: %w", line, err)
	}
	return c, nil
}
