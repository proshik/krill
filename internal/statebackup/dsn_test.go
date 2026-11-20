package statebackup

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func sorted(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	g, w := sorted(got), sorted(want)
	if strings.Join(g, "\n") != strings.Join(w, "\n") {
		t.Fatalf("env mismatch\n got: %q\nwant: %q", g, w)
	}
}

func tmpFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.pem")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConnFromDSNURL(t *testing.T) {
	c, err := ConnFromDSN("postgres://krill:pw@127.0.0.1:5432/krill?sslmode=disable", nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, c.Env, "PGHOST=127.0.0.1", "PGPORT=5432", "PGUSER=krill", "PGPASSWORD=pw", "PGDATABASE=krill", "PGSSLMODE=disable")
	if len(c.Files) != 0 {
		t.Fatalf("files: %v", c.Files)
	}
}

func TestConnFromDSNKeywordForm(t *testing.T) {
	c, err := ConnFromDSN("host=db.example.com port=6432 user=u password=p dbname=d sslmode=require connect_timeout=5", nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, c.Env, "PGHOST=db.example.com", "PGPORT=6432", "PGUSER=u", "PGPASSWORD=p", "PGDATABASE=d", "PGSSLMODE=require", "PGCONNECT_TIMEOUT=5")
}

func TestConnFromDSNPasswordDecoding(t *testing.T) {
	c, err := ConnFromDSN("postgres://u:p%40ss%2Fw@h/d?sslmode=disable", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGPASSWORD=p@ss/w") {
		t.Fatalf("env: %q", c.Env)
	}
	c, err = ConnFromDSN(`host=h user=u dbname=d password='a b\'c'`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGPASSWORD=a b'c") {
		t.Fatalf("env: %q", c.Env)
	}
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

func TestConnFromDSNMultiHost(t *testing.T) {
	c, err := ConnFromDSN("postgres://u:p@h1:5432,h2:5433/d?sslmode=disable", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGHOST=h1,h2") || !contains(c.Env, "PGPORT=5432,5433") {
		t.Fatalf("env: %q", c.Env)
	}
}

func TestConnFromDSNAllowList(t *testing.T) {
	c, err := ConnFromDSN("postgres://u:p@h/d?options=-c%20search_path%3Dkrill&target_session_attrs=read-write&application_name=x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGOPTIONS=-c search_path=krill") || !contains(c.Env, "PGTARGETSESSIONATTRS=read-write") {
		t.Fatalf("env: %q", c.Env)
	}
	for _, e := range c.Env {
		if strings.Contains(e, "application_name") || strings.Contains(strings.ToUpper(e), "APPLICATION") {
			t.Fatalf("unexpected: %q", e)
		}
	}
}

func TestConnFromDSNNoSSLModeStaysAbsent(t *testing.T) {
	c, err := ConnFromDSN("postgres://u:p@h/d", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Env {
		if strings.HasPrefix(e, "PGSSLMODE=") {
			t.Fatalf("unexpected %q", e)
		}
	}
}

func TestConnFromDSNRefusals(t *testing.T) {
	cases := []struct {
		dsn  string
		want error
	}{
		{"host=/var/run/postgresql user=u password=pw dbname=d", ErrUnixSocket},
		{"host=h user=u password=pw dbname=d sslpassword=x", ErrSSLPassword},
		{"::not a dsn", ErrDSNParse},
	}
	for _, tc := range cases {
		_, err := ConnFromDSN(tc.dsn, nil)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%q: got %v want %v", tc.dsn, err, tc.want)
		}
		if strings.Contains(err.Error(), tc.dsn) || strings.Contains(err.Error(), "pw") {
			t.Fatalf("error leaks input: %v", err)
		}
	}
}

func TestConnFromDSNSSLFiles(t *testing.T) {
	p := tmpFile(t)
	c, err := ConnFromDSN("postgres://u:p@h/d?sslmode=verify-full&sslrootcert="+p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGSSLROOTCERT="+p) || len(c.Files) != 1 || c.Files[0] != p {
		t.Fatalf("env %q files %v", c.Env, c.Files)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := ConnFromDSN("postgres://u:p@h/d?sslrootcert="+missing, nil); !errors.Is(err, ErrSSLFile) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := ConnFromDSN("postgres://u:p@h/d?sslrootcert="+t.TempDir(), nil); !errors.Is(err, ErrSSLFile) {
		t.Fatalf("dir: %v", err)
	}
	c, err = ConnFromDSN("postgres://u:p@h/d?sslrootcert=system", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGSSLROOTCERT=system") || len(c.Files) != 0 {
		t.Fatalf("env %q files %v", c.Env, c.Files)
	}
}

func TestConnFromDSNVerifyFullUsesCABundle(t *testing.T) {
	p := tmpFile(t)
	missing := filepath.Join(t.TempDir(), "nope")
	c, err := ConnFromDSN("postgres://u:p@h/d?sslmode=verify-full", []string{missing, p})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGSSLROOTCERT="+p) || len(c.Files) != 1 || c.Files[0] != p {
		t.Fatalf("env %q files %v", c.Env, c.Files)
	}
	if _, err := ConnFromDSN("postgres://u:p@h/d?sslmode=verify-full", []string{missing}); !errors.Is(err, ErrNoCABundle) {
		t.Fatalf("got %v", err)
	}
	c, err = ConnFromDSN("postgres://u:p@h/d?sslmode=require", []string{missing})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Env {
		if strings.HasPrefix(e, "PGSSLROOTCERT=") {
			t.Fatalf("unexpected %q", e)
		}
	}
}

func TestConnFromDSNRelativeSSLPathRefused(t *testing.T) {
	for _, p := range []string{"sslrootcert", "sslcert", "sslkey"} {
		if _, err := ConnFromDSN("postgres://u:p@h/d?"+p+"=rel/file.pem", nil); !errors.Is(err, ErrSSLFile) {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func TestConnFromDSNClientCertPassthroughAndDedupe(t *testing.T) {
	cert, key := tmpFile(t), tmpFile(t)
	c, err := ConnFromDSN("postgres://u:p@h/d?sslcert="+cert+"&sslkey="+key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGSSLCERT="+cert) || !contains(c.Env, "PGSSLKEY="+key) || len(c.Files) != 2 {
		t.Fatalf("env %q files %v", c.Env, c.Files)
	}
	c, err = ConnFromDSN("postgres://u:p@h/d?sslcert="+cert+"&sslkey="+cert, nil)
	if err != nil || len(c.Files) != 1 {
		t.Fatalf("err %v files %v", err, c.Files)
	}
	c, err = ConnFromDSN("postgres://u:p@h/d?sslmode=verify-full&sslcert="+cert, []string{cert})
	if err != nil || len(c.Files) != 1 {
		t.Fatalf("err %v files %v", err, c.Files)
	}
	missing := filepath.Join(t.TempDir(), "nokey")
	if _, err := ConnFromDSN("postgres://u:p@h/d?sslcert="+cert+"&sslkey="+missing, nil); !errors.Is(err, ErrSSLFile) {
		t.Fatalf("missing key: %v", err)
	}
}
