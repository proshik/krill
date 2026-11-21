package accept

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ExcludedTables are the tables whose data a state backup leaves out
// (statebackup.DumpArgs): they are not compared, and neither are the
// sequences they own.
var ExcludedTables = []string{"metric_samples", "sessions"}

// IgnoredColumns are rewritten by Krill on its own while it runs, so they
// differ between any two moments: the metrics sampler's columns on every
// tick, and a deployment's log, which the log cleanup empties an hour after
// the deploy finished. They are left out of every table hash.
var IgnoredColumns = map[string][]string{
	"deployments":   {"log"},
	"node_capacity": {"sampled_at"},
	"node_disks":    {"avail_bytes", "sampled_at", "used_bytes"},
}

// Options narrow one comparison: Ignore leaves more columns of a table out
// of its hash, Where keeps only the rows matching a SQL condition (for
// "existing rows unchanged, new rows checked separately").
type Options struct {
	Ignore map[string][]string
	Where  map[string]string
}

// FingerprintSettings makes timestamps and floats render the same in every
// container; run it on the same session before FingerprintQuery.
const FingerprintSettings = "SET TimeZone = 'UTC'; SET extra_float_digits = 1;"

// TablesSQL lists the public tables to fingerprint, one per row, in order.
func TablesSQL() string {
	return "SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> ALL(" +
		textArray(ExcludedTables) + ") ORDER BY 1"
}

// FingerprintSQL is FingerprintSettings followed by FingerprintQueryOpts,
// for psql.
func FingerprintSQL(tables, schemaSkip []string, opts Options) string {
	return FingerprintSettings + "\n" + FingerprintQueryOpts(tables, schemaSkip, opts) + ";"
}

// FingerprintQuery returns one text column, "line", one row per fingerprint
// part, ordered:
//
//	T <tab> table <tab> rows <tab> md5   for every table in tables
//	S <tab> sequence <tab> last_value    for every public sequence not owned by an excluded table
//	H <tab> md5                          the schema: columns, constraints, indexes
//
// A row's text is its jsonb form minus the ignored columns, so neither the
// physical column order nor the row order changes a hash. schemaSkip tables
// are left out of the schema hash (a table a later migration adds).
func FingerprintQuery(tables, schemaSkip []string) string {
	return FingerprintQueryOpts(tables, schemaSkip, Options{})
}

// FingerprintQueryOpts is FingerprintQuery narrowed by opts.
func FingerprintQueryOpts(tables, schemaSkip []string, opts Options) string {
	var parts []string
	for _, t := range tables {
		ignore := append(append([]string(nil), IgnoredColumns[t]...), opts.Ignore[t]...)
		where := ""
		if w := opts.Where[t]; w != "" {
			where = " WHERE " + w
		}
		parts = append(parts, fmt.Sprintf(
			"SELECT 'T' || E'\\t' || %s || E'\\t' || count(*) || E'\\t' || md5(coalesce(string_agg(r, E'\\n' ORDER BY r), '')) AS line "+
				"FROM (SELECT (to_jsonb(x) - %s)::text AS r FROM public.%s x%s) s",
			quoteLiteral(t), textArray(ignore), quoteIdent(t), where))
	}
	parts = append(parts, "SELECT 'S' || E'\\t' || s.sequencename || E'\\t' || coalesce(s.last_value::text, '') AS line "+
		"FROM pg_sequences s WHERE s.schemaname = 'public' AND NOT EXISTS ("+
		"SELECT 1 FROM pg_depend d JOIN pg_class c ON c.oid = d.refobjid "+
		"WHERE d.classid = 'pg_class'::regclass AND d.objid = (quote_ident(s.schemaname) || '.' || quote_ident(s.sequencename))::regclass "+
		"AND d.deptype IN ('a', 'i') AND c.relname = ANY("+textArray(ExcludedTables)+"))")
	skip := textArray(schemaSkip)
	parts = append(parts, "SELECT 'H' || E'\\t' || md5(coalesce(string_agg(h, E'\\n' ORDER BY h), '')) AS line FROM ("+
		"SELECT 'C ' || table_name || ' ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '') AS h "+
		"FROM information_schema.columns WHERE table_schema = 'public' AND table_name <> ALL("+skip+") "+
		"UNION ALL SELECT 'K ' || rel.relname || ' ' || con.conname || ' ' || pg_get_constraintdef(con.oid) "+
		"FROM pg_constraint con JOIN pg_class rel ON rel.oid = con.conrelid JOIN pg_namespace n ON n.oid = rel.relnamespace "+
		"WHERE n.nspname = 'public' AND rel.relname <> ALL("+skip+") "+
		"UNION ALL SELECT 'I ' || tablename || ' ' || indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename <> ALL("+skip+")"+
		") hs")
	return "SELECT line FROM (" + strings.Join(parts, " UNION ALL ") + ") fp ORDER BY line"
}

// TableSum is one table's part of a fingerprint.
type TableSum struct {
	Rows int64
	Hash string
}

// Fingerprint is a database's content as the restore checks compare it.
type Fingerprint struct {
	Tables    map[string]TableSum
	Sequences map[string]string
	Schema    string
}

// ParseFingerprint reads FingerprintSQL's output as `psql -X -At` prints it
// (the "SET" echo lines are skipped).
func ParseFingerprint(out string) (Fingerprint, error) {
	fp := Fingerprint{Tables: map[string]TableSum{}, Sequences: map[string]string{}}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.TrimSpace(line) == "" || strings.TrimSpace(line) == "SET" {
			continue
		}
		f := strings.Split(line, "\t")
		switch {
		case f[0] == "T" && len(f) == 4:
			n, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil {
				return fp, fmt.Errorf("fingerprint line %q: %w", line, err)
			}
			fp.Tables[f[1]] = TableSum{Rows: n, Hash: f[3]}
		case f[0] == "S" && len(f) == 3:
			fp.Sequences[f[1]] = f[2]
		case f[0] == "H" && len(f) == 2:
			fp.Schema = f[1]
		default:
			return fp, fmt.Errorf("unexpected fingerprint line %q", line)
		}
	}
	if fp.Schema == "" {
		return fp, fmt.Errorf("fingerprint has no schema line")
	}
	return fp, nil
}

// Difference is one part where two fingerprints disagree. Object is
// "table", "sequence" or "schema"; Kind is "changed", "missing" (only in
// the first) or "extra" (only in the second).
type Difference struct {
	Object, Kind, Name, A, B string
}

func (d Difference) String() string {
	return fmt.Sprintf("%s %s %s: %s -> %s", d.Object, d.Name, d.Kind, d.A, d.B)
}

// Diff lists where b differs from a, sorted by object and name.
func Diff(a, b Fingerprint) []Difference {
	var out []Difference
	sum := func(s TableSum) string { return fmt.Sprintf("rows=%d hash=%s", s.Rows, s.Hash) }
	for name, x := range a.Tables {
		y, ok := b.Tables[name]
		switch {
		case !ok:
			out = append(out, Difference{"table", "missing", name, sum(x), ""})
		case x != y:
			out = append(out, Difference{"table", "changed", name, sum(x), sum(y)})
		}
	}
	for name, y := range b.Tables {
		if _, ok := a.Tables[name]; !ok {
			out = append(out, Difference{"table", "extra", name, "", sum(y)})
		}
	}
	for name, x := range a.Sequences {
		y, ok := b.Sequences[name]
		switch {
		case !ok:
			out = append(out, Difference{"sequence", "missing", name, x, ""})
		case x != y:
			out = append(out, Difference{"sequence", "changed", name, x, y})
		}
	}
	for name, y := range b.Sequences {
		if _, ok := a.Sequences[name]; !ok {
			out = append(out, Difference{"sequence", "extra", name, "", y})
		}
	}
	if a.Schema != b.Schema {
		out = append(out, Difference{"schema", "changed", "public", a.Schema, b.Schema})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Object != out[j].Object {
			return out[i].Object < out[j].Object
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// secretColumn reports whether a column may hold a credential and must be
// left out of a row dump.
func secretColumn(c string) bool {
	if c == "id" || strings.HasSuffix(c, "_id") {
		return false
	}
	for _, w := range []string{"password", "secret", "token", "key"} {
		if strings.Contains(c, w) {
			return true
		}
	}
	return false
}

// RowsSQL dumps a table's rows as jsonb text, sorted, for the evidence of a
// fingerprint mismatch; every column whose name suggests a credential is
// removed first. columns are the table's column names.
func RowsSQL(table string, columns []string) string {
	var drop []string
	for _, c := range columns {
		if secretColumn(c) {
			drop = append(drop, c)
		}
	}
	return fmt.Sprintf("SELECT (to_jsonb(x) - %s)::text FROM public.%s x ORDER BY 1", textArray(drop), quoteIdent(table))
}

// ColumnsSQL lists a public table's column names, one per row.
func ColumnsSQL(table string) string {
	return "SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = " +
		quoteLiteral(table) + " ORDER BY ordinal_position"
}

func quoteIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func textArray(items []string) string {
	q := make([]string, len(items))
	for i, it := range items {
		q[i] = quoteLiteral(it)
	}
	return "ARRAY[" + strings.Join(q, ", ") + "]::text[]"
}
