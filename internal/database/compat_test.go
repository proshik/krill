package database

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestCompatViolations(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string // substrings expected among the reported violations, or nil for none
	}{
		{
			name: "drop column explicit",
			sql:  `ALTER TABLE applications DROP COLUMN old_field;`,
			want: []string{"DROP COLUMN"},
		},
		{
			name: "drop column bare (COLUMN keyword omitted)",
			sql:  `ALTER TABLE applications DROP old_field;`,
			want: []string{"DROP COLUMN"},
		},
		{
			name: "drop table",
			sql:  `DROP TABLE legacy_widgets;`,
			want: []string{"DROP TABLE"},
		},
		{
			name: "rename column",
			sql:  `ALTER TABLE applications RENAME COLUMN foo TO bar;`,
			want: []string{"RENAME"},
		},
		{
			name: "rename table",
			sql:  `ALTER TABLE widgets RENAME TO gadgets;`,
			want: []string{"RENAME"},
		},
		{
			name: "alter column type",
			sql:  `ALTER TABLE applications ALTER COLUMN port TYPE bigint;`,
			want: []string{"ALTER COLUMN ... TYPE"},
		},
		{
			name: "alter column set not null",
			sql:  `ALTER TABLE applications ALTER COLUMN port SET NOT NULL;`,
			want: []string{"ALTER COLUMN ... SET NOT NULL"},
		},
		{
			name: "add column explicit not null without default",
			sql:  `ALTER TABLE applications ADD COLUMN required_field TEXT NOT NULL;`,
			want: []string{"ADD COLUMN ... NOT NULL without DEFAULT"},
		},
		{
			name: "add column bare (COLUMN keyword omitted) not null without default",
			sql:  `ALTER TABLE applications ADD required_field TEXT NOT NULL;`,
			want: []string{"ADD COLUMN ... NOT NULL without DEFAULT"},
		},
		{
			name: "add column bare not null with default",
			sql:  `ALTER TABLE applications ADD required_field TEXT NOT NULL DEFAULT '';`,
			want: nil,
		},
		{
			name: "add column not null with default after",
			sql:  `ALTER TABLE applications ADD COLUMN x TEXT NOT NULL DEFAULT '';`,
			want: nil,
		},
		{
			name: "add column not null with default before",
			sql:  `ALTER TABLE applications ADD COLUMN x TEXT DEFAULT '' NOT NULL;`,
			want: nil,
		},
		{
			name: "add column nullable",
			sql:  `ALTER TABLE applications ADD COLUMN optional_field TEXT;`,
			want: nil,
		},
		{
			name: "add column bare nullable",
			sql:  `ALTER TABLE applications ADD optional_field TEXT;`,
			want: nil,
		},
		{
			name: "create table with not null is fine",
			sql:  `CREATE TABLE widgets (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL);`,
			want: nil,
		},
		{
			name: "drop constraint alone is never a violation",
			sql:  `ALTER TABLE applications DROP CONSTRAINT applications_port_chk;`,
			want: nil,
		},
		{
			name: "add constraint check with no matching drop is a new check",
			sql:  `ALTER TABLE t ADD CONSTRAINT t_new_chk CHECK (x > 0);`,
			want: []string{"ADD CONSTRAINT ... CHECK"},
		},
		{
			name: "unnamed add check is always a new check",
			sql:  `ALTER TABLE t ADD CHECK (x > 0);`,
			want: []string{"ADD CONSTRAINT ... CHECK"},
		},
		{
			name: "drop and recreate the same named check widens without a marker",
			sql: `ALTER TABLE app_db_links DROP CONSTRAINT app_db_links_field_chk;
ALTER TABLE app_db_links ADD CONSTRAINT app_db_links_field_chk
    CHECK (field IN ('url','host','port','endpoint'));`,
			want: nil,
		},
		{
			name: "new table with defaults is fine",
			sql:  `CREATE TABLE widgets (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL DEFAULT '');`,
			want: nil,
		},
		{
			name: "new index is fine",
			sql:  `CREATE INDEX idx_applications_name ON applications (name);`,
			want: nil,
		},
		{
			name: "new non-unique index concurrently is fine",
			sql:  `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_applications_name ON applications (name);`,
			want: nil,
		},
		// A new uniqueness or foreign key can make the previous release's
		// writes fail, so none of them is additive.
		{
			name: "create unique index",
			sql:  `CREATE UNIQUE INDEX idx_domains_host ON domains (host);`,
			want: []string{"CREATE UNIQUE INDEX"},
		},
		{
			name: "create unique index concurrently if not exists",
			sql:  `create unique index concurrently if not exists idx_domains_host on domains (host);`,
			want: []string{"CREATE UNIQUE INDEX"},
		},
		{
			name: "add named unique constraint",
			sql:  `ALTER TABLE domains ADD CONSTRAINT domains_host_key UNIQUE (host);`,
			want: []string{"ADD CONSTRAINT ... UNIQUE"},
		},
		{
			name: "add quoted named unique constraint using an index",
			sql:  `ALTER TABLE domains ADD CONSTRAINT "domains_host_key" UNIQUE USING INDEX idx_domains_host;`,
			want: []string{"ADD CONSTRAINT ... UNIQUE"},
		},
		{
			name: "add unnamed unique constraint",
			sql:  `ALTER TABLE domains ADD UNIQUE (host);`,
			want: []string{"ADD CONSTRAINT ... UNIQUE"},
		},
		{
			name: "add named foreign key",
			sql:  `ALTER TABLE applications ADD CONSTRAINT applications_registry_fk FOREIGN KEY (registry_id) REFERENCES registries (id);`,
			want: []string{"ADD CONSTRAINT ... FOREIGN KEY"},
		},
		{
			name: "add unnamed foreign key",
			sql:  `ALTER TABLE applications ADD FOREIGN KEY (registry_id) REFERENCES registries (id) ON DELETE SET NULL;`,
			want: []string{"ADD CONSTRAINT ... FOREIGN KEY"},
		},
		{
			name: "drop table only inside a comment is fine",
			sql:  "-- DROP TABLE IF EXISTS old_table\nCREATE TABLE widgets (id BIGSERIAL PRIMARY KEY);",
			want: nil,
		},
		{
			name: "marker with reason exempts only its own statement",
			sql: `-- krill:compat-break-ok safe once v0.5.0 no longer ships
ALTER TABLE applications DROP COLUMN old_field;
ALTER TABLE applications DROP COLUMN another_field;`,
			want: []string{"DROP COLUMN"}, // only the second, unmarked statement
		},
		{
			name: "marker without reason is itself a violation",
			sql: `-- krill:compat-break-ok
ALTER TABLE applications DROP COLUMN old_field;`,
			want: []string{"marker present without a reason"},
		},
		{
			name: "marker text inside a quoted identifier is not a comment and does not exempt",
			sql:  `ALTER TABLE t DROP COLUMN "krill:compat-break-ok reason";`,
			want: []string{"DROP COLUMN"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := compatViolations(c.sql)
			if c.want == nil {
				if len(got) != 0 {
					t.Fatalf("compatViolations(%q) = %v, want none", c.sql, got)
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("compatViolations(%q) = %v, want %v", c.sql, got, c.want)
			}
			for i, w := range c.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("compatViolations(%q)[%d] = %q, want substring %q", c.sql, i, got[i], w)
				}
			}
		})
	}
}

func TestMigrationVersion(t *testing.T) {
	cases := []struct {
		name   string
		wantV  uint
		wantOK bool
	}{
		{"000046_panel_hsts.up.sql", 46, true},
		{"000001_init.up.sql", 1, true},
		{"README.md", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		v, ok := migrationVersion(c.name)
		if ok != c.wantOK || (ok && v != c.wantV) {
			t.Errorf("migrationVersion(%q) = (%d, %v), want (%d, %v)", c.name, v, ok, c.wantV, c.wantOK)
		}
	}
}

// TestSweepCompatPolicyReportsViolationsAboveThreshold exercises
// sweepCompatPolicy against a synthetic fs.FS so the sweep itself is proven
// against a real violating file, not just against the (currently clean)
// embedded migrations.
func TestSweepCompatPolicyReportsViolationsAboveThreshold(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/000046_y.up.sql":     {Data: []byte(`ALTER TABLE t DROP COLUMN old_field;`)},                       // at the threshold: skipped
		"migrations/000046_y.down.sql":   {Data: []byte(`ALTER TABLE t ADD COLUMN old_field TEXT;`)},                   // .down.sql: always ignored
		"migrations/000047_x.up.sql":     {Data: []byte(`ALTER TABLE t DROP COLUMN old_field;`)},                       // above the threshold: reported
		"migrations/000047_x.down.sql":   {Data: []byte(`ALTER TABLE t DROP COLUMN old_field; -- would also violate`)}, // .down.sql: always ignored
		"migrations/000048_clean.up.sql": {Data: []byte(`ALTER TABLE t ADD COLUMN note TEXT;`)},                        // above the threshold, clean
	}

	problems, err := sweepCompatPolicy(fsys)
	if err != nil {
		t.Fatalf("sweepCompatPolicy: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("sweepCompatPolicy() = %v, want exactly 1 problem", problems)
	}
	want := "000047_x.up.sql: DROP COLUMN"
	if problems[0] != want {
		t.Fatalf("sweepCompatPolicy()[0] = %q, want %q", problems[0], want)
	}
}

// TestMigrationsRespectCompatPolicy sweeps every embedded *.up.sql file
// numbered above compatPolicyStart and fails, listing every violation with
// its file name and matched pattern, if any breaks the expand/contract
// compatibility policy (CLAUDE.md §4, docs/development.md "Migrations").
// Today no migration is above the threshold, so this passes vacuously —
// TestCompatViolations and TestSweepCompatPolicyReportsViolationsAboveThreshold
// above are what exercise the matcher and the sweep itself.
func TestMigrationsRespectCompatPolicy(t *testing.T) {
	problems, err := sweepCompatPolicy(migrationsFS)
	if err != nil {
		t.Fatalf("sweepCompatPolicy: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("migrations violate the compatibility policy:\n%s", strings.Join(problems, "\n"))
	}
}
