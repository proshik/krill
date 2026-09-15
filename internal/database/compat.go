package database

import (
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// compatPolicyStart is the last migration version that predates the
// expand/contract compatibility policy (see CLAUDE.md §4 and
// docs/development.md). Only *.up.sql files numbered higher than this are
// swept by the lint: a migration must keep the previous release's binary
// bootable, since self-update rolls back to it on a failed upgrade.
const compatPolicyStart = 46

// compatBreakMarker exempts one SQL statement from the lint below when a
// comment line naming it, with a reason, precedes that statement (e.g.
// "-- krill:compat-break-ok safe once v0.5.0 no longer ships").
const compatBreakMarker = "krill:compat-break-ok"

// compatComment strips a SQL line comment starting with "--".
var compatComment = regexp.MustCompile(`--[^\n]*`)

// compatPatterns are the breaking-change shapes the lint reports via a
// simple whole-statement match. DROP CONSTRAINT and ADD CONSTRAINT ... CHECK
// are handled separately (see addConstraintCheckViolation) because whether
// they're a break depends on the rest of the file, not just the statement
// itself; DROP COLUMN and ADD COLUMN ... NOT NULL are also handled
// separately because Postgres allows the COLUMN keyword to be omitted.
//
// A new uniqueness (CREATE UNIQUE INDEX, ADD [CONSTRAINT <name>] UNIQUE) or
// foreign key (ADD [CONSTRAINT <name>] FOREIGN KEY) is flagged whatever the
// rest of the file does: either can make a write the previous release still
// performs fail.
var compatPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"DROP TABLE", regexp.MustCompile(`(?is)\bDROP\s+TABLE\b`)},
	{"RENAME", regexp.MustCompile(`(?is)\bRENAME\b`)},
	{"ALTER COLUMN ... TYPE", regexp.MustCompile(`(?is)\bALTER\s+COLUMN\b[^,;]*?\bTYPE\b`)},
	{"ALTER COLUMN ... SET NOT NULL", regexp.MustCompile(`(?is)\bALTER\s+COLUMN\b[^,;]*?\bSET\s+NOT\s+NULL\b`)},
	{"CREATE UNIQUE INDEX", regexp.MustCompile(`(?is)\bCREATE\s+UNIQUE\s+INDEX\b`)},
	{"ADD CONSTRAINT ... UNIQUE", regexp.MustCompile(`(?is)\bADD\s+(?:CONSTRAINT\s+(?:"[^"]+"|\w+)\s+)?UNIQUE\b`)},
	{"ADD CONSTRAINT ... FOREIGN KEY", regexp.MustCompile(`(?is)\bADD\s+(?:CONSTRAINT\s+(?:"[^"]+"|\w+)\s+)?FOREIGN\s+KEY\b`)},
}

// compatDropColumnExplicit matches the unambiguous "DROP COLUMN ..." form.
var compatDropColumnExplicit = regexp.MustCompile(`(?is)\bDROP\s+COLUMN\b`)

// compatDropWord matches "DROP <word>" so the word can be checked against
// compatNonColumnDropTargets to tell a bare "DROP <column>" (Postgres allows
// omitting the COLUMN keyword) from "DROP TABLE ...", "DROP INDEX ...", etc.
// Go's regexp (RE2) has no lookahead, so this two-step match-then-check in
// Go is the way to keep the object-type keywords out of the column check.
var compatDropWord = regexp.MustCompile(`(?is)\bDROP\s+(\w+)`)

// compatNonColumnDropTargets are the object-type keywords that can follow
// DROP without meaning "drop this column".
var compatNonColumnDropTargets = map[string]bool{
	"COLUMN": true, "TABLE": true, "CONSTRAINT": true, "INDEX": true,
	"DEFAULT": true, "NOT": true, "IF": true, "SCHEMA": true, "EXTENSION": true,
	"TRIGGER": true, "POLICY": true, "VIEW": true, "SEQUENCE": true, "TYPE": true,
	"FUNCTION": true, "PROCEDURE": true, "RULE": true, "DOMAIN": true, "ROLE": true,
	"USER": true, "DATABASE": true, "OWNED": true, "MATERIALIZED": true,
	"PUBLICATION": true, "SUBSCRIPTION": true, "STATISTICS": true, "COLLATION": true,
	"CONVERSION": true, "OPERATOR": true, "AGGREGATE": true, "CAST": true,
	"LANGUAGE": true, "SERVER": true, "FOREIGN": true, "ACCESS": true, "EVENT": true,
}

// hasDropColumn reports whether stmt drops a column, in either the explicit
// "DROP COLUMN x" form or the bare "DROP x" form Postgres also accepts.
func hasDropColumn(stmt string) bool {
	if compatDropColumnExplicit.MatchString(stmt) {
		return true
	}
	for _, m := range compatDropWord.FindAllStringSubmatch(stmt, -1) {
		if !compatNonColumnDropTargets[strings.ToUpper(m[1])] {
			return true
		}
	}
	return false
}

// compatAddWord matches "ADD [COLUMN] <word>" so the word can be checked
// against compatNonColumnAddTargets to tell a bare "ADD <column> <type>"
// (Postgres allows omitting the COLUMN keyword) from "ADD CONSTRAINT ...",
// "ADD CHECK (...)", "ADD PRIMARY KEY (...)", etc.
var compatAddWord = regexp.MustCompile(`(?is)\bADD\s+(?:COLUMN\s+)?(\w+)`)

// compatNonColumnAddTargets are the keywords that can follow ADD (or
// ADD COLUMN's absence) without meaning "add this column".
var compatNonColumnAddTargets = map[string]bool{
	"CONSTRAINT": true, "CHECK": true, "PRIMARY": true, "UNIQUE": true,
	"FOREIGN": true, "EXCLUDE": true,
}

var compatNotNull = regexp.MustCompile(`(?is)\bNOT\s+NULL\b`)
var compatDefault = regexp.MustCompile(`(?is)\bDEFAULT\b`)

// hasAddColumnNotNullWithoutDefault reports whether stmt adds a column that
// is NOT NULL without also carrying a DEFAULT in the same clause (checked
// order-independently: DEFAULT may appear before or after NOT NULL).
func hasAddColumnNotNullWithoutDefault(stmt string) bool {
	for _, clause := range addColumnClauses(stmt) {
		if compatNotNull.MatchString(clause) && !compatDefault.MatchString(clause) {
			return true
		}
	}
	return false
}

// addColumnClauses returns the "ADD [COLUMN] ..." clauses in stmt, each
// extended from its ADD keyword up to the next comma (or the end of stmt),
// skipping ADD forms that don't add a column (ADD CONSTRAINT, ADD CHECK,
// ADD PRIMARY KEY, ADD UNIQUE, ADD FOREIGN KEY, ADD EXCLUDE).
func addColumnClauses(stmt string) []string {
	var clauses []string
	for _, m := range compatAddWord.FindAllStringSubmatchIndex(stmt, -1) {
		word := strings.ToUpper(stmt[m[2]:m[3]])
		if compatNonColumnAddTargets[word] {
			continue
		}
		rest := stmt[m[0]:]
		if idx := strings.IndexByte(rest, ','); idx >= 0 {
			rest = rest[:idx]
		}
		clauses = append(clauses, rest)
	}
	return clauses
}

// compatDropConstraintName matches "DROP CONSTRAINT [IF EXISTS] <name>" and
// captures the (unquoted or quoted) constraint name.
var compatDropConstraintName = regexp.MustCompile(`(?is)\bDROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?`)

// compatAddConstraintCheckNamed matches "ADD CONSTRAINT <name> CHECK (...)"
// and captures the constraint name.
var compatAddConstraintCheckNamed = regexp.MustCompile(`(?is)\bADD\s+CONSTRAINT\s+"?(\w+)"?\s+CHECK\b`)

// compatAddCheckUnnamed matches the unnamed "ADD CHECK (...)" short form.
var compatAddCheckUnnamed = regexp.MustCompile(`(?is)\bADD\s+CHECK\s*\(`)

// droppedConstraintNames collects every constraint name dropped anywhere in
// the (comment-stripped) migration text, upper-cased for case-insensitive
// comparison.
func droppedConstraintNames(strippedFile string) map[string]bool {
	dropped := map[string]bool{}
	for _, m := range compatDropConstraintName.FindAllStringSubmatch(strippedFile, -1) {
		dropped[strings.ToUpper(m[1])] = true
	}
	return dropped
}

// addConstraintCheckViolation reports whether stmt adds a new CHECK
// constraint that isn't a drop-and-recreate of a same-named one elsewhere in
// the file. A DROP CONSTRAINT by itself is never a violation — removing a
// constraint cannot make an old release's writes fail — so the repo's
// drop-then-add-the-same-name-with-a-wider-CHECK pattern (e.g. migrations
// 000033, 000036, 000037, 000038) passes without a marker; only a CHECK
// added under a name the file never drops (including every unnamed
// "ADD CHECK (...)") counts as new and is flagged, since the lint has no way
// to tell a widened re-creation from a genuinely tightened or brand-new one.
func addConstraintCheckViolation(stmt string, droppedInFile map[string]bool) bool {
	if compatAddCheckUnnamed.MatchString(stmt) {
		return true
	}
	if m := compatAddConstraintCheckNamed.FindStringSubmatch(stmt); m != nil {
		return !droppedInFile[strings.ToUpper(m[1])]
	}
	return false
}

// splitStatements splits raw SQL text into ";"-terminated statements. This
// is a simple heuristic (it does not understand string literals that
// contain a semicolon), sufficient for the migration files in this repo.
func splitStatements(sql string) []string {
	parts := strings.Split(sql, ";")
	var stmts []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			stmts = append(stmts, p)
		}
	}
	return stmts
}

// statementMarker scans a raw (comments intact) statement chunk for a
// compatBreakMarker comment line — a line whose first non-blank characters
// are "--" followed by the marker — and reports whether the statement is
// exempt (the marker carries a reason) and whether a bare marker (no reason)
// was found, which is itself a violation. A marker that isn't its own
// comment line (e.g. inside a string literal, or trailing after code on the
// same line) is not recognized.
func statementMarker(stmt string) (exempt bool, bareMarker bool) {
	for _, line := range strings.Split(stmt, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "--") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
		if !strings.HasPrefix(rest, compatBreakMarker) {
			continue
		}
		reason := strings.TrimSpace(strings.TrimPrefix(rest, compatBreakMarker))
		if reason != "" {
			return true, false
		}
		return false, true
	}
	return false, false
}

// statementViolations returns the distinct breaking-change patterns matched
// by one comment-stripped statement.
func statementViolations(stmt string, droppedInFile map[string]bool) []string {
	var names []string
	add := func(name string) {
		for _, n := range names {
			if n == name {
				return
			}
		}
		names = append(names, name)
	}

	for _, p := range compatPatterns {
		if p.re.MatchString(stmt) {
			add(p.name)
		}
	}
	if hasDropColumn(stmt) {
		add("DROP COLUMN")
	}
	if hasAddColumnNotNullWithoutDefault(stmt) {
		add("ADD COLUMN ... NOT NULL without DEFAULT")
	}
	if addConstraintCheckViolation(stmt, droppedInFile) {
		add("ADD CONSTRAINT ... CHECK")
	}
	return names
}

// compatViolations scans raw migration SQL text and reports one message per
// statement that breaks the expand/contract compatibility policy: DROP
// COLUMN (with or without the COLUMN keyword), DROP TABLE, RENAME (column or
// table), ALTER COLUMN ... TYPE, ALTER COLUMN ... SET NOT NULL, ADD COLUMN
// ... NOT NULL (with or without the COLUMN keyword) without a DEFAULT in the
// same clause, CREATE UNIQUE INDEX, a new UNIQUE or FOREIGN KEY constraint
// (named or not), and a new CHECK constraint (named or not) that isn't a
// drop-and-recreate of a same-named one elsewhere in the file — DROP
// CONSTRAINT by itself is never flagged. Comments are stripped before
// matching, so a pattern appearing only inside a "-- ..." comment is
// ignored. The lint cannot tell a widened re-creation from a genuinely
// tightened one, so it still takes a human reading the migration.
//
// The exemption marker is scoped to one statement, not the whole file: SQL
// text is first split into ";"-terminated statements, and a statement
// preceded by its own "-- krill:compat-break-ok <reason>" comment line is
// skipped entirely; a bare marker with no reason is itself reported as a
// violation for that statement.
func compatViolations(sql string) []string {
	droppedInFile := droppedConstraintNames(compatComment.ReplaceAllString(sql, ""))

	var violations []string
	for _, stmt := range splitStatements(sql) {
		exempt, bareMarker := statementMarker(stmt)
		if bareMarker {
			violations = append(violations, fmt.Sprintf("%s marker present without a reason on the same line", compatBreakMarker))
			continue
		}
		if exempt {
			continue
		}
		names := statementViolations(compatComment.ReplaceAllString(stmt, ""), droppedInFile)
		if len(names) > 0 {
			violations = append(violations, strings.Join(names, ", "))
		}
	}
	return violations
}

// migrationVersion parses the leading decimal digits of a migration file
// name (e.g. "000047_add_widgets.up.sql" -> 47, true).
func migrationVersion(name string) (uint, bool) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	v, err := strconv.ParseUint(name[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	return uint(v), true
}

// sweepCompatPolicy checks every *.up.sql file above compatPolicyStart
// against compatViolations and returns one formatted message per violation,
// naming the offending file and matched pattern(s).
func sweepCompatPolicy(migrations fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}

	var problems []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		v, ok := migrationVersion(e.Name())
		if !ok || v <= compatPolicyStart {
			continue
		}
		content, err := fs.ReadFile(migrations, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		for _, violation := range compatViolations(string(content)) {
			problems = append(problems, fmt.Sprintf("%s: %s", e.Name(), violation))
		}
	}
	return problems, nil
}
