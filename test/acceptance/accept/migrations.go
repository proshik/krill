// Package accept holds the pure helpers of the live acceptance harness in
// test/acceptance: migration numbering for the throwaway releases, the
// database fingerprint the restore checks compare, and the list of runbook
// commands pinned to docs/install.md. It has no build tag, so its unit tests
// run in `make test`; the tagged harness imports it.
package accept

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
)

var upMigrationRe = regexp.MustCompile(`^(\d{6})_.+\.up\.sql$`)

// HighestMigration returns the highest migration number among the
// NNNNNN_*.up.sql files in dir; an error when there is none.
func HighestMigration(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, e := range entries {
		m := upMigrationRe.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		highest = max(highest, n)
	}
	if highest == 0 {
		return 0, fmt.Errorf("no NNNNNN_*.up.sql migration in %s", dir)
	}
	return highest, nil
}

// MigrationFile names migration n with stem and suffix (".up.sql" or
// ".down.sql"), as golang-migrate expects it.
func MigrationFile(n int, stem, suffix string) string {
	return fmt.Sprintf("%06d_%s%s", n, stem, suffix)
}
