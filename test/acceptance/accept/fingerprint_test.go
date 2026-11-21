package accept

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/proshik/krill/internal/testutil"
)

// fingerprintOf runs the SQL the harness runs through psql, over pgx, and
// joins the rows the way `psql -At` prints them.
func fingerprintOf(t *testing.T, pool *pgxpool.Pool, schemaSkip ...string) Fingerprint {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, FingerprintSettings); err != nil {
		t.Fatal(err)
	}
	tables := queryLines(t, conn, TablesSQL())
	lines := queryLines(t, conn, FingerprintQuery(tables, schemaSkip))
	fp, err := ParseFingerprint(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func queryLines(t *testing.T, conn *pgxpool.Conn, sql string) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func TestFingerprintDeterministic(t *testing.T) {
	pool := testutil.NewTestDB(t)
	a, b := fingerprintOf(t, pool), fingerprintOf(t, pool)
	if d := Diff(a, b); len(d) != 0 {
		t.Fatalf("two fingerprints of one database differ: %v", d)
	}
	if _, ok := a.Tables["users"]; !ok {
		t.Fatalf("users not fingerprinted: %v", a.Tables)
	}
	for _, ex := range ExcludedTables {
		if _, ok := a.Tables[ex]; ok {
			t.Fatalf("excluded table %s fingerprinted", ex)
		}
	}
}

func TestFingerprintSeesRowChange(t *testing.T) {
	pool := testutil.NewTestDB(t)
	before := fingerprintOf(t, pool)
	exec(t, pool, "INSERT INTO users (email, password_hash) VALUES ('fp@accept.test', 'x')")
	d := Diff(before, fingerprintOf(t, pool))
	if len(d) != 2 || d[0].Object != "sequence" || d[0].Name != "users_id_seq" || d[1].Object != "table" || d[1].Name != "users" {
		t.Fatalf("differences %v, want the users table and users_id_seq", d)
	}
}

func TestFingerprintStableUnderSamplerWrites(t *testing.T) {
	pool := testutil.NewTestDB(t)
	exec(t, pool, "INSERT INTO node_capacity (node, ncpu, mem_total_bytes, sampled_at) VALUES ('n1', 2, 100, now() - interval '1 hour')")
	exec(t, pool, "INSERT INTO node_disks (node, mount, filesystem, total_bytes, used_bytes, avail_bytes, sampled_at) VALUES ('n1', '/', 'ext4', 100, 10, 90, now() - interval '1 hour')")
	before := fingerprintOf(t, pool)
	exec(t, pool, "UPDATE node_capacity SET sampled_at = now()")
	exec(t, pool, "UPDATE node_disks SET used_bytes = 20, avail_bytes = 80, sampled_at = now()")
	if d := Diff(before, fingerprintOf(t, pool)); len(d) != 0 {
		t.Fatalf("sampler-only writes changed the fingerprint: %v", d)
	}
	exec(t, pool, "UPDATE node_capacity SET ncpu = 4")
	if d := Diff(before, fingerprintOf(t, pool)); len(d) != 1 || d[0].Name != "node_capacity" {
		t.Fatalf("a real node_capacity change gave %v", d)
	}
}

func TestFingerprintIgnoresExcludedTables(t *testing.T) {
	pool := testutil.NewTestDB(t)
	before := fingerprintOf(t, pool)
	exec(t, pool, "INSERT INTO metric_samples (component, cpu_pct, mem_bytes) VALUES ('c', 1, 1)")
	if d := Diff(before, fingerprintOf(t, pool)); len(d) != 0 {
		t.Fatalf("a metric_samples insert changed the fingerprint: %v", d)
	}
}

func TestFingerprintColumnOrderIndependent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	exec(t, pool, "CREATE TABLE t1 (a int, b text); CREATE TABLE t2 (b text, a int)")
	exec(t, pool, "INSERT INTO t1 VALUES (1, 'x'), (2, 'y'); INSERT INTO t2 VALUES ('y', 2), ('x', 1)")
	fp := fingerprintOf(t, pool)
	if fp.Tables["t1"] != fp.Tables["t2"] {
		t.Fatalf("t1 %v != t2 %v", fp.Tables["t1"], fp.Tables["t2"])
	}
}

func TestFingerprintEmptyTable(t *testing.T) {
	pool := testutil.NewTestDB(t)
	exec(t, pool, "CREATE TABLE t_empty (a int)")
	got := fingerprintOf(t, pool).Tables["t_empty"]
	if got.Rows != 0 || got.Hash != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("empty table: %+v, want 0 rows and md5('')", got)
	}
}

func TestFingerprintSchemaSkip(t *testing.T) {
	pool := testutil.NewTestDB(t)
	before := fingerprintOf(t, pool, "acceptance_sbk_forward")
	exec(t, pool, "CREATE TABLE acceptance_sbk_forward (id BIGSERIAL PRIMARY KEY)")
	if d := Diff(before, fingerprintOf(t, pool)); !hasDiff(d, "schema", "public") {
		t.Fatalf("schema change not seen without skip: %v", d)
	}
	d := Diff(before, fingerprintOf(t, pool, "acceptance_sbk_forward"))
	if hasDiff(d, "schema", "public") || !hasDiff(d, "table", "acceptance_sbk_forward") {
		t.Fatalf("with skip: %v, want only the new table (and its sequence), no schema difference", d)
	}
}

func hasDiff(d []Difference, object, name string) bool {
	for _, x := range d {
		if x.Object == object && x.Name == name {
			return true
		}
	}
	return false
}

func TestParseFingerprintPsqlOutput(t *testing.T) {
	out := "SET\nSET\nH\tabc\nS\tusers_id_seq\t3\nS\tunused_seq\t\nT\tusers\t3\tdef\n"
	fp, err := ParseFingerprint(out)
	if err != nil {
		t.Fatal(err)
	}
	if fp.Schema != "abc" || fp.Sequences["users_id_seq"] != "3" || fp.Sequences["unused_seq"] != "" || fp.Tables["users"] != (TableSum{3, "def"}) {
		t.Fatalf("parsed %+v", fp)
	}
	if _, err := ParseFingerprint("H\tabc\nX\tbroken\n"); err == nil {
		t.Fatal("malformed line accepted")
	}
	if _, err := ParseFingerprint("T\tusers\t3\tdef\n"); err == nil {
		t.Fatal("output without a schema line accepted")
	}
}

func TestRowsSQLOmitsSecretColumns(t *testing.T) {
	sql := RowsSQL("x", []string{"id", "org_id", "password", "bot_token", "secret_key", "access_key", "name"})
	for _, dropped := range []string{"'password'", "'bot_token'", "'secret_key'", "'access_key'"} {
		if !strings.Contains(sql, dropped) {
			t.Errorf("RowsSQL does not drop %s: %s", dropped, sql)
		}
	}
	for _, kept := range []string{"'id'", "'org_id'", "'name'"} {
		if strings.Contains(sql, kept) {
			t.Errorf("RowsSQL drops %s: %s", kept, sql)
		}
	}
}

// fingerprintOpts is fingerprintOf with per-comparison options.
func fingerprintOpts(t *testing.T, pool *pgxpool.Pool, opts Options) Fingerprint {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, FingerprintSettings); err != nil {
		t.Fatal(err)
	}
	lines := queryLines(t, conn, FingerprintQueryOpts(queryLines(t, conn, TablesSQL()), nil, opts))
	fp, err := ParseFingerprint(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestFingerprintOptionsIgnoreAndWhere(t *testing.T) {
	pool := testutil.NewTestDB(t)
	exec(t, pool, "CREATE TABLE t_opts (id int PRIMARY KEY, a text, b text); INSERT INTO t_opts VALUES (1, 'x', 'y')")
	opts := Options{Ignore: map[string][]string{"t_opts": {"b"}}, Where: map[string]string{"t_opts": "id <= 1"}}
	before := fingerprintOpts(t, pool, opts)
	exec(t, pool, "UPDATE t_opts SET b = 'changed'; INSERT INTO t_opts VALUES (2, 'new', 'row')")
	if d := Diff(before, fingerprintOpts(t, pool, opts)); len(d) != 0 {
		t.Fatalf("an ignored column and a row outside Where changed the fingerprint: %v", d)
	}
	exec(t, pool, "UPDATE t_opts SET a = 'changed' WHERE id = 1")
	if d := Diff(before, fingerprintOpts(t, pool, opts)); len(d) != 1 || d[0].Name != "t_opts" {
		t.Fatalf("a compared column change gave %v", d)
	}
}

func TestDeploymentLogIgnored(t *testing.T) {
	for _, c := range IgnoredColumns["deployments"] {
		if c == "log" {
			return
		}
	}
	t.Fatal("deployments.log is emptied by Krill's log cleanup an hour after a deploy; it must be ignored")
}
