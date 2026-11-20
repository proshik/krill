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
}

// pg_dump 13–15 reads sslrootcert=system as a file name: it becomes the
// host's CA bundle with verify-full, which is what the pool does with it.
func TestConnFromDSNSystemRootCert(t *testing.T) {
	ca := tmpFile(t)
	missing := filepath.Join(t.TempDir(), "nope")
	for _, dsn := range []string{
		"postgres://u:p@h/d?sslrootcert=system",
		"postgres://u:p@h/d?sslrootcert=system&sslmode=require",
		"host=h user=u password=p dbname=d sslrootcert=system sslmode=verify-ca",
	} {
		c, err := connFromDSN(dsn, []string{missing, ca}, envOf(nil))
		if err != nil {
			t.Fatalf("%q: %v", dsn, err)
		}
		if !contains(c.Env, "PGSSLROOTCERT="+ca) || !contains(c.Env, "PGSSLMODE=verify-full") || len(c.Files) != 1 || c.Files[0] != ca {
			t.Fatalf("%q: env %q files %v", dsn, c.Env, c.Files)
		}
		for _, e := range c.Env {
			if e == "PGSSLROOTCERT=system" || (strings.HasPrefix(e, "PGSSLMODE=") && e != "PGSSLMODE=verify-full") {
				t.Fatalf("%q: unexpected %q", dsn, e)
			}
		}
	}
	c, err := connFromDSN("postgres://u:p@h/d", []string{ca}, envOf(map[string]string{"PGSSLROOTCERT": "system"}))
	if err != nil || !contains(c.Env, "PGSSLROOTCERT="+ca) || !contains(c.Env, "PGSSLMODE=verify-full") {
		t.Fatalf("env: err %v env %q", err, c.Env)
	}
	if _, err := connFromDSN("postgres://u:p@h/d?sslrootcert=system", []string{missing}, envOf(nil)); !errors.Is(err, ErrNoCABundle) {
		t.Fatalf("no bundle: %v", err)
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

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConnFromDSNInheritsTLSFromEnvironment(t *testing.T) {
	ca, cert, key := tmpFile(t), tmpFile(t), tmpFile(t)
	env := envOf(map[string]string{
		"PGSSLMODE": "verify-full", "PGSSLROOTCERT": ca, "PGSSLCERT": cert, "PGSSLKEY": key,
		"PGCONNECT_TIMEOUT": "7",
	})
	c, err := connFromDSN("postgres://u:p@h/d", nil, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PGSSLMODE=verify-full", "PGSSLROOTCERT=" + ca, "PGSSLCERT=" + cert, "PGSSLKEY=" + key, "PGCONNECT_TIMEOUT=7"} {
		if !contains(c.Env, want) {
			t.Fatalf("missing %q in %q", want, c.Env)
		}
	}
	if len(c.Files) != 3 {
		t.Fatalf("files %v", c.Files)
	}

	// The DSN's own value wins, as it does for the pool.
	c, err = connFromDSN("postgres://u:p@h/d?sslmode=require", nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGSSLMODE=require") || contains(c.Env, "PGSSLMODE=verify-full") {
		t.Fatalf("env %q", c.Env)
	}

	// verify-full from the environment without a root cert uses the CA bundle.
	c, err = connFromDSN("host=h user=u password=p dbname=d", []string{ca}, envOf(map[string]string{"PGSSLMODE": "verify-full"}))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Env, "PGSSLMODE=verify-full") || !contains(c.Env, "PGSSLROOTCERT="+ca) {
		t.Fatalf("env %q", c.Env)
	}
	if _, err := connFromDSN("postgres://u:p@h/d", nil, envOf(map[string]string{"PGSSLMODE": "verify-ca"})); !errors.Is(err, ErrNoCABundle) {
		t.Fatalf("got %v", err)
	}
}

func TestConnFromDSNEnvironmentSSLFilesChecked(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	for _, k := range []string{"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY"} {
		for _, v := range []string{missing, "rel/file.pem", t.TempDir()} {
			if _, err := connFromDSN("postgres://u:p@h/d", nil, envOf(map[string]string{k: v})); !errors.Is(err, ErrSSLFile) {
				t.Fatalf("%s=%s: %v", k, v, err)
			}
		}
	}
}

func TestConnFromDSNRefusesServiceFileAndEnvSSLPassword(t *testing.T) {
	for _, dsn := range []string{"postgres://u:p@h/d?service=krill", "service=krill user=u password=pw"} {
		_, err := connFromDSN(dsn, nil, envOf(nil))
		if !errors.Is(err, ErrServiceFile) || strings.Contains(err.Error(), "pw") {
			t.Fatalf("%q: %v", dsn, err)
		}
	}
	if _, err := connFromDSN("postgres://u:p@h/d", nil, envOf(map[string]string{"PGSERVICE": "krill"})); !errors.Is(err, ErrServiceFile) {
		t.Fatalf("PGSERVICE: %v", err)
	}
	if _, err := connFromDSN("postgres://u:p@h/d", nil, envOf(map[string]string{"PGSSLPASSWORD": "x"})); !errors.Is(err, ErrSSLPassword) {
		t.Fatalf("PGSSLPASSWORD: %v", err)
	}
}

// pgconn reads Krill's PGSSL* environment while parsing; the parse must not
// load a certificate file that only the dump container reads.
func TestConnFromDSNParseIgnoresEnvironmentCertificates(t *testing.T) {
	notPEM := tmpFile(t)
	t.Setenv("PGSSLROOTCERT", notPEM)
	t.Setenv("PGSSLMODE", "verify-ca")
	for _, dsn := range []string{"postgres://u:p@h/d", "host=h user=u password=p dbname=d"} {
		c, err := ConnFromDSN(dsn, nil)
		if err != nil {
			t.Fatalf("%q: %v", dsn, err)
		}
		if !contains(c.Env, "PGSSLROOTCERT="+notPEM) || !contains(c.Env, "PGSSLMODE=verify-ca") {
			t.Fatalf("env %q", c.Env)
		}
	}
}

// pgx takes the first value of a repeated URL parameter and the last of a
// repeated keyword; the dump must never pick a different one, so a repeated
// connection or TLS parameter is refused in either order and either form.
func TestConnFromDSNRefusesRepeatedParameters(t *testing.T) {
	ca := tmpFile(t)
	for _, dsn := range []string{
		"postgres://u:pw@h/d?sslmode=verify-full&sslmode=disable",
		"postgres://u:pw@h/d?sslmode=disable&sslmode=verify-full",
		"postgres://u:pw@h/d?sslmode=verify-full&sslmode=require",
		"postgres://u:pw@h/d?sslmode=require&sslmode=verify-full",
		"postgres://u:pw@h/d?sslrootcert=" + ca + "&sslrootcert=system",
		"postgres://u:pw@h/d?channel_binding=require&channel_binding=disable",
		"postgres://u:pw@h/d?sslnegotiation=direct&sslnegotiation=postgres",
		"postgres://u:pw@h/d?options=-c%20a%3D1&options=-c%20a%3D2",
		"host=h user=u password=pw dbname=d sslmode=verify-full sslmode=disable",
		"host=h user=u password=pw dbname=d sslmode=disable sslmode=verify-full",
		"host=h user=u password=pw dbname=d channel_binding=require channel_binding=disable",
	} {
		_, err := connFromDSN(dsn, []string{ca}, envOf(nil))
		if !errors.Is(err, ErrRepeated) {
			t.Fatalf("%q: got %v", dsn, err)
		}
		if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "verify") {
			t.Fatalf("error leaks input: %v", err)
		}
	}
	// A repeated parameter the dump does not use stays accepted.
	if _, err := connFromDSN("postgres://u:p@h/d?sslmode=disable&application_name=a&application_name=b", nil, envOf(nil)); err != nil {
		t.Fatal(err)
	}
}

// The pool honors sslnegotiation (DSN or PGSSLNEGOTIATION) and channel_binding;
// the dump must too, or it negotiates TLS the old way or falls back to prefer.
func TestConnFromDSNNegotiationAndChannelBinding(t *testing.T) {
	c, err := connFromDSN("postgres://u:p@h/d?sslmode=require&sslnegotiation=direct&channel_binding=require", nil, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PGSSLMODE=require", "PGSSLNEGOTIATION=direct", "PGCHANNELBINDING=require"} {
		if !contains(c.Env, want) {
			t.Fatalf("missing %q in %q", want, c.Env)
		}
	}
	if c.MinClientMajor != 17 {
		t.Fatalf("MinClientMajor = %d", c.MinClientMajor)
	}

	// From Krill's environment, and the default prefer becomes require as
	// it does in the pool.
	c, err = connFromDSN("host=h user=u password=p dbname=d", nil, envOf(map[string]string{"PGSSLNEGOTIATION": "direct", "PGCHANNELBINDING": "require"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PGSSLMODE=require", "PGSSLNEGOTIATION=direct", "PGCHANNELBINDING=require"} {
		if !contains(c.Env, want) {
			t.Fatalf("missing %q in %q", want, c.Env)
		}
	}

	// postgres negotiation and channel binding need no particular client.
	c, err = connFromDSN("postgres://u:p@h/d?sslnegotiation=postgres&channel_binding=require", nil, envOf(nil))
	if err != nil || c.MinClientMajor != 0 || !contains(c.Env, "PGSSLNEGOTIATION=postgres") {
		t.Fatalf("err %v env %q min %d", err, c.Env, c.MinClientMajor)
	}

	// With disable the pool sends no TLS, so there is nothing to negotiate.
	c, err = connFromDSN("postgres://u:p@h/d?sslmode=disable&sslnegotiation=direct", nil, envOf(nil))
	if err != nil || c.MinClientMajor != 0 {
		t.Fatalf("err %v min %d", err, c.MinClientMajor)
	}
	for _, e := range c.Env {
		if strings.HasPrefix(e, "PGSSLNEGOTIATION=") {
			t.Fatalf("unexpected %q", e)
		}
	}
	if _, err := connFromDSN("postgres://u:p@h/d?sslmode=allow&sslnegotiation=direct", nil, envOf(nil)); !errors.Is(err, ErrWeakDirect) {
		t.Fatalf("allow: %v", err)
	}
}

// The pool's system roots honor SSL_CERT_FILE (replaces the default bundle)
// and SSL_CERT_DIR (adds a directory the dump cannot reproduce).
func TestConnFromDSNSystemRootsFollowSSLCertEnv(t *testing.T) {
	bundle, custom := tmpFile(t), tmpFile(t)
	for _, dsn := range []string{"postgres://u:p@h/d?sslmode=verify-full", "postgres://u:p@h/d?sslrootcert=system"} {
		c, err := connFromDSN(dsn, []string{bundle}, envOf(map[string]string{"SSL_CERT_FILE": custom}))
		if err != nil {
			t.Fatalf("%q: %v", dsn, err)
		}
		if !contains(c.Env, "PGSSLROOTCERT="+custom) || len(c.Files) != 1 || c.Files[0] != custom {
			t.Fatalf("%q: env %q files %v", dsn, c.Env, c.Files)
		}
		for _, v := range []string{filepath.Join(t.TempDir(), "nope"), "rel/ca.pem", t.TempDir()} {
			if _, err := connFromDSN(dsn, []string{bundle}, envOf(map[string]string{"SSL_CERT_FILE": v})); !errors.Is(err, ErrCertFile) {
				t.Fatalf("%q SSL_CERT_FILE=%s: %v", dsn, v, err)
			}
		}
		for _, env := range []map[string]string{
			{"SSL_CERT_DIR": t.TempDir()},
			{"SSL_CERT_DIR": t.TempDir(), "SSL_CERT_FILE": custom},
		} {
			if _, err := connFromDSN(dsn, []string{bundle}, envOf(env)); !errors.Is(err, ErrCertDir) {
				t.Fatalf("%q %v: %v", dsn, env, err)
			}
		}
	}
	// Neither matters when the bundle is not needed.
	c, err := connFromDSN("postgres://u:p@h/d?sslmode=verify-full&sslrootcert="+custom, nil, envOf(map[string]string{"SSL_CERT_DIR": t.TempDir(), "SSL_CERT_FILE": "rel"}))
	if err != nil || !contains(c.Env, "PGSSLROOTCERT="+custom) {
		t.Fatalf("explicit root cert: err %v env %q", err, c.Env)
	}
	if _, err := connFromDSN("postgres://u:p@h/d?sslmode=require", nil, envOf(map[string]string{"SSL_CERT_DIR": t.TempDir()})); err != nil {
		t.Fatalf("require: %v", err)
	}
}
