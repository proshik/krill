// Package statebackup backs up Krill's own control-plane Postgres.
package statebackup

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ConnEnv is a libpq environment derived from a DSN.
type ConnEnv struct {
	Env   []string // "PGHOST=…", … — PGPASSWORD included; never log
	Files []string // host paths to bind-mount read-only at the same path
}

// DefaultCABundles is checked in order when sslmode is verify-ca/verify-full
// and the DSN names no sslrootcert.
var DefaultCABundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem"}

// Sentinel errors. Their messages are fixed strings: a pgconn error echoes the
// DSN, so none of them ever wraps one.
var (
	ErrDSNParse    = errors.New("the database URL could not be parsed")
	ErrUnixSocket  = errors.New("the database URL uses a unix socket, which the dump container cannot reach")
	ErrSSLPassword = errors.New("the database URL uses sslpassword, which is not supported")
	ErrNoCABundle  = errors.New("sslmode needs a CA bundle but none was found on this host and the URL has no sslrootcert")
	ErrSSLFile     = errors.New("an SSL file named in the database URL or a PGSSL* environment variable is missing or not a regular file")
	ErrServiceFile = errors.New("the database connection relies on a PostgreSQL service file (service= or PGSERVICE), which the dump container cannot reproduce; put its settings in KRILL_DATABASE_URL")
)

// allowed maps the libpq parameters that are passed through to their
// environment variables.
var allowed = []struct{ param, env string }{
	{"sslmode", "PGSSLMODE"},
	{"sslrootcert", "PGSSLROOTCERT"},
	{"sslcert", "PGSSLCERT"},
	{"sslkey", "PGSSLKEY"},
	{"connect_timeout", "PGCONNECT_TIMEOUT"},
	{"target_session_attrs", "PGTARGETSESSIONATTRS"},
	{"options", "PGOPTIONS"},
}

var fileParams = map[string]bool{"sslrootcert": true, "sslcert": true, "sslkey": true}

// ConnFromDSN maps a Postgres DSN to libpq environment variables so the
// password reaches pg_dump through the environment, never through argv.
//
// The control-plane pool (pgconn) takes a parameter missing from the DSN from
// Krill's own PG* environment, and the dump container does not inherit that
// environment, so the passed-through parameters fall back to it here too.
func ConnFromDSN(dsn string, caBundles []string) (ConnEnv, error) {
	return connFromDSN(dsn, caBundles, os.Getenv)
}

func connFromDSN(dsn string, caBundles []string, getenv func(string) string) (ConnEnv, error) {
	raw, err := rawParams(dsn)
	if err != nil {
		return ConnEnv{}, err
	}
	if _, ok := raw["sslpassword"]; ok || getenv("PGSSLPASSWORD") != "" {
		return ConnEnv{}, ErrSSLPassword
	}
	if _, ok := raw["service"]; ok || getenv("PGSERVICE") != "" {
		return ConnEnv{}, ErrServiceFile
	}
	// eff holds what the pool uses: the DSN's value (even an empty one),
	// else the environment's.
	eff := map[string]string{}
	for _, a := range allowed {
		if v, ok := raw[a.param]; ok {
			eff[a.param] = v
		} else {
			eff[a.param] = getenv(a.env)
		}
	}
	// Checked before pgconn, which would reject a missing file with an
	// error that echoes the DSN.
	var out ConnEnv
	for _, a := range allowed {
		v := eff[a.param]
		if v == "" || !fileParams[a.param] || (a.param == "sslrootcert" && v == "system") {
			continue
		}
		if !filepath.IsAbs(v) {
			return ConnEnv{}, ErrSSLFile
		}
		if st, err := os.Stat(v); err != nil || !st.Mode().IsRegular() {
			return ConnEnv{}, ErrSSLFile
		}
		out.Files = appendUnique(out.Files, v)
	}
	cfg, err := pgconn.ParseConfig(withoutTLS(dsn, raw))
	if err != nil {
		return ConnEnv{}, ErrDSNParse
	}

	var hosts, ports []string
	seen := map[string]bool{}
	add := func(h string, p uint16) error {
		if strings.HasPrefix(h, "/") {
			return ErrUnixSocket
		}
		k := h + ":" + strconv.Itoa(int(p))
		if seen[k] {
			return nil
		}
		seen[k] = true
		hosts = append(hosts, h)
		ports = append(ports, strconv.Itoa(int(p)))
		return nil
	}
	if err := add(cfg.Host, cfg.Port); err != nil {
		return ConnEnv{}, err
	}
	for _, f := range cfg.Fallbacks {
		if err := add(f.Host, f.Port); err != nil {
			return ConnEnv{}, err
		}
	}

	out.Env = []string{
		"PGHOST=" + strings.Join(hosts, ","),
		"PGPORT=" + strings.Join(ports, ","),
		"PGUSER=" + cfg.User,
		"PGPASSWORD=" + cfg.Password,
		"PGDATABASE=" + cfg.Database,
	}

	for _, a := range allowed {
		if v := eff[a.param]; v != "" {
			out.Env = append(out.Env, a.env+"="+v)
		}
	}

	mode := eff["sslmode"]
	if (mode == "verify-ca" || mode == "verify-full") && eff["sslrootcert"] == "" {
		found := ""
		for _, p := range caBundles {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				found = p
				break
			}
		}
		if found == "" {
			return ConnEnv{}, ErrNoCABundle
		}
		out.Env = append(out.Env, "PGSSLROOTCERT="+found)
		out.Files = appendUnique(out.Files, found)
	}
	return out, nil
}

// withoutTLS returns dsn with every TLS parameter replaced by sslmode=disable
// and empty file parameters, which also override Krill's PGSSL* environment.
// pgconn would otherwise load the certificate files while parsing and reject
// content that only the dump container will read; TLS settings reach the
// container from raw and the environment, not from the parsed config.
func withoutTLS(dsn string, raw map[string]string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		for _, k := range []string{"sslrootcert", "sslcert", "sslkey", "sslpassword"} {
			q.Set(k, "")
		}
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
		return u.String()
	}
	var b strings.Builder
	for k, v := range raw {
		switch k {
		case "sslrootcert", "sslcert", "sslkey", "sslpassword", "sslmode":
			continue
		}
		r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
		b.WriteString(k + "='" + r.Replace(v) + "' ")
	}
	b.WriteString("sslrootcert='' sslcert='' sslkey='' sslpassword='' sslmode=disable")
	return b.String()
}

// rawParams returns the libpq parameters of either DSN form, decoded.
func rawParams(dsn string) (map[string]string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, ErrDSNParse
		}
		m := map[string]string{}
		for k, v := range u.Query() {
			if len(v) > 0 {
				m[k] = v[len(v)-1]
			}
		}
		return m, nil
	}
	m, err := parseKeywords(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w", ErrDSNParse)
	}
	return m, nil
}

// parseKeywords parses libpq keyword/value syntax: key=value pairs separated
// by whitespace, values optionally single-quoted, backslash escaping the next
// character.
func parseKeywords(s string) (map[string]string, error) {
	m := map[string]string{}
	i, n := 0, len(s)
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for {
		for i < n && space(s[i]) {
			i++
		}
		if i >= n {
			return m, nil
		}
		start := i
		for i < n && s[i] != '=' && !space(s[i]) {
			i++
		}
		key := s[start:i]
		for i < n && space(s[i]) {
			i++
		}
		if key == "" || i >= n || s[i] != '=' {
			return nil, ErrDSNParse
		}
		i++
		for i < n && space(s[i]) {
			i++
		}
		var val strings.Builder
		if i < n && s[i] == '\'' {
			i++
			closed := false
			for i < n {
				c := s[i]
				if c == '\\' && i+1 < n {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				val.WriteByte(c)
				i++
			}
			if !closed {
				return nil, ErrDSNParse
			}
		} else {
			for i < n && !space(s[i]) {
				if s[i] == '\\' && i+1 < n {
					i++
				}
				val.WriteByte(s[i])
				i++
			}
		}
		m[key] = val.String()
	}
}

func appendUnique(s []string, v string) []string {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}
