package dbservice

import (
	"strings"
	"testing"
)

func TestGenerateAppName(t *testing.T) {
	a := GenerateAppName("postgres", "My DB")
	if !strings.HasPrefix(a, "krill-postgres-my-db-") {
		t.Errorf("prefix wrong: %q", a)
	}
	// suffix of 6 hex characters
	parts := strings.Split(a, "-")
	suf := parts[len(parts)-1]
	if len(suf) != 6 {
		t.Errorf("suffix len = %d (%q)", len(suf), suf)
	}
	// uniqueness
	b := GenerateAppName("postgres", "My DB")
	if a == b {
		t.Error("app names must be unique")
	}
}

func TestGenerateAppNameEmptyName(t *testing.T) {
	a := GenerateAppName("redis", "")
	if !strings.HasPrefix(a, "krill-redis-") {
		t.Errorf("prefix wrong: %q", a)
	}
}
