package accept

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHighestMigration(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"000001_a.up.sql", "000001_a.down.sql", "000062_x.up.sql", "000062_x.down.sql",
		"README.md", "0007_bad.up.sql", "000099_only_down.down.sql",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := HighestMigration(dir)
	if err != nil || got != 62 {
		t.Fatalf("HighestMigration = %d, %v; want 62, nil", got, err)
	}
}

func TestHighestMigrationEmpty(t *testing.T) {
	if n, err := HighestMigration(t.TempDir()); err == nil {
		t.Fatalf("HighestMigration of an empty dir = %d, nil; want an error", n)
	}
}

func TestHighestMigrationRepo(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "internal", "database", "migrations")
	got, err := HighestMigration(dir)
	if err != nil {
		t.Fatal(err)
	}
	ups, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if got != len(ups) || got < 62 {
		t.Fatalf("highest migration %d, %d up files: want them equal (contiguous) and >= 62", got, len(ups))
	}
}

func TestMigrationFile(t *testing.T) {
	if got := MigrationFile(63, "acceptance_a", ".up.sql"); got != "000063_acceptance_a.up.sql" {
		t.Fatalf("MigrationFile = %q", got)
	}
}
