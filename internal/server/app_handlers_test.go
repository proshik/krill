package server

import (
	"context"
	"reflect"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestIsSlug(t *testing.T) {
	ok := []string{"web", "my-app", "a1", "x-y-z"}
	bad := []string{"", "Web", "my_app", "a b", "café"}
	for _, s := range ok {
		if !isSlug(s) {
			t.Errorf("isSlug(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isSlug(s) {
			t.Errorf("isSlug(%q) = true, want false", s)
		}
	}
}

func TestParseEnv(t *testing.T) {
	got, dups := parseEnv("FOO=bar\n  BAZ = qux \n\nINVALID\nK=v=w\nFOO=again")
	want := map[string]string{"FOO": "again", "BAZ": "qux", "K": "v=w"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseEnv = %#v, want %#v", got, want)
	}
	if len(dups) != 1 || dups[0] != "FOO" {
		t.Errorf("parseEnv dups = %#v, want [FOO]", dups)
	}
}

// The port-conflict checks discarded their query errors, so a transient DB
// failure read as "port is free": two DB instances (or an instance and an app's
// raw published port) could then be created on the same host port, and the
// collision only surfaced later as a service that will not publish.
func TestPortConflictChecksFailClosed(t *testing.T) {
	pool := testutil.NewTestDB(t)
	s := &Server{q: db.New(pool)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stand-in for any query failure (pool exhausted, deadline, ...)

	if _, err := s.dbInstancePortInUse(ctx, 5432); err == nil {
		t.Error("dbInstancePortInUse swallowed a query failure and reported the port free")
	}
	if _, err := s.dbInstancePortInUseByOther(ctx, 5432, 1); err == nil {
		t.Error("dbInstancePortInUseByOther swallowed a query failure and reported the port free")
	}
}
