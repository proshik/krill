package dbservice

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestValidIdent(t *testing.T) {
	ok := []string{"shop", "_x", "a1_b2", "a"}
	bad := []string{"", "1abc", "Shop", "a-b", "a b", "тест", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} // 64 chars
	for _, s := range ok {
		if !ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = true, want false", s)
		}
	}
}

func TestSanitizeIdent(t *testing.T) {
	cases := map[string]string{"My Shop": "my_shop", "1abc": "db_1abc", "app-db": "app_db", "ПРИВЕТ": "db"}
	for in, want := range cases {
		if got := SanitizeIdent(in); got != want {
			t.Errorf("SanitizeIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateDBSQL(t *testing.T) {
	got := createDBSQL("shop", "shop_u", "p_w-9")
	want := "CREATE USER \"shop_u\" PASSWORD 'p_w-9';\n" +
		"CREATE DATABASE \"shop\" OWNER \"shop_u\";\n" +
		"REVOKE CONNECT ON DATABASE \"shop\" FROM PUBLIC;\n"
	if got != want {
		t.Fatalf("createDBSQL:\n%q\nwant:\n%q", got, want)
	}
}

func TestDropDBSQL(t *testing.T) {
	got := dropDBSQL("shop", "shop_u")
	want := "DROP DATABASE IF EXISTS \"shop\" WITH (FORCE);\nDROP USER IF EXISTS \"shop_u\";\n"
	if got != want {
		t.Fatalf("dropDBSQL:\n%q\nwant:\n%q", got, want)
	}
}

// scriptedEngine answers each psql exec by the statement it receives on
// stdin and records what ran.
type scriptedEngine struct {
	*mockEngine
	ran    []string
	exists bool
}

func (e *scriptedEngine) Exec(_ context.Context, _ string, cmd []string, _ []string, stdin io.Reader, out io.Writer) error {
	b, _ := io.ReadAll(stdin)
	sql := string(b)
	e.ran = append(e.ran, sql)
	if strings.Contains(sql, "krill: objects exist") && e.exists {
		// Like docker.Engine.Exec: psql's stderr arrives in the error text.
		return errors.New("exec [psql] exited 3: ERROR:  krill: objects exist")
	}
	if strings.HasPrefix(sql, "CREATE USER") {
		return errors.New("exited 3")
	}
	if !slices.Contains(cmd, "-X") {
		return errors.New("psql run without -X")
	}
	return nil
}

// A database or role that already exists on the server must be refused and
// left alone: the compensating drop used to run after the failed CREATE and
// delete a database Krill never created (e.g. one restored from a dump).
func TestProvisionNeverDropsAnExistingDatabase(t *testing.T) {
	eng := &scriptedEngine{mockEngine: newMockEngine(), exists: true}
	svc := New(eng, nil, nil, "")
	err := svc.ProvisionLogicalDB(t.Context(), Instance{AppName: "pg"}, LogicalDB{DBName: "app", Username: "app", Password: "pw"})
	if !errors.Is(err, ErrObjectsExist) {
		t.Fatalf("err = %v, want ErrObjectsExist", err)
	}
	for _, sql := range eng.ran {
		if strings.Contains(sql, "DROP") || strings.Contains(sql, "CREATE USER") {
			t.Fatalf("ran %q against an existing database", sql)
		}
	}
}

// A failure after the check (nothing existed) still compensates.
func TestProvisionCompensatesItsOwnFailure(t *testing.T) {
	eng := &scriptedEngine{mockEngine: newMockEngine()}
	svc := New(eng, nil, nil, "")
	if err := svc.ProvisionLogicalDB(t.Context(), Instance{AppName: "pg"}, LogicalDB{DBName: "app", Username: "app", Password: "pw"}); err == nil {
		t.Fatal("want the create failure")
	}
	if len(eng.ran) != 3 || !strings.Contains(eng.ran[2], "DROP DATABASE") {
		t.Fatalf("ran = %q, want check, create, compensating drop", eng.ran)
	}
}
