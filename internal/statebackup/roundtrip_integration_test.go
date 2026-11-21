//go:build integration

package statebackup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/testutil"
)

const rtPrefix = "ops"

// rtRunner records the images the real engine was asked to run.
type rtRunner struct {
	docker.OneOffRunner
	mu     sync.Mutex
	images []string
}

func (r *rtRunner) RunOneOff(ctx context.Context, spec docker.OneOffSpec, out io.Writer) error {
	r.mu.Lock()
	r.images = append(r.images, spec.Image)
	r.mu.Unlock()
	return r.OneOffRunner.RunOneOff(ctx, spec, out)
}

func (r *rtRunner) used() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.images...)
}

// rtEngine is the real engine, type-asserted the way cmd/krill does.
func rtEngine(t *testing.T) docker.OneOffRunner {
	t.Helper()
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	r, ok := eng.(docker.OneOffRunner)
	if !ok {
		t.Fatalf("engine %T does not implement docker.OneOffRunner", eng)
	}
	return r
}

// rtLocalDSN rewrites a testcontainer DSN to localhost:<mapped port>, which
// the dump container's host network reaches (on CI and inside Colima's VM).
func rtLocalDSN(t *testing.T, dsn, password, query string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort("localhost", u.Port())
	u.User = url.UserPassword(u.User.Username(), password)
	u.RawQuery = query
	return u.String()
}

// rtLabel gives svc a dump-container label value of its own, so the leftover
// checks below count and remove only this test's containers, never a live
// Krill's dump running on the same Docker daemon.
func rtLabel(t *testing.T, svc *Service) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	svc.labelValue = fmt.Sprintf("%s-test-%x", StateDumpLabelValue, b)
	return svc.labelValue
}

type rtFixture struct {
	pool   *pgxpool.Pool // the source database (also holds the settings)
	dsn    string        // the source DSN as Krill would see it
	runner *rtRunner
	dst    backup.Destination
	id     *age.X25519Identity
	svc    *Service
	label  string // the fixture's dump-container label value
}

// newRTFixture starts the source database and MinIO, and saves enabled
// settings for a fresh age recipient. dsnPassword is the password put into
// the DSN the service dumps with.
func newRTFixture(t *testing.T, dsnPassword string) *rtFixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.NewTestDB(t)
	dsn := rtLocalDSN(t, pool.Config().ConnString(), dsnPassword, "sslmode=disable")

	mi := testutil.NewMinio(t)
	dst := backup.Destination{
		Endpoint: mi.Endpoint, Bucket: "krill-state-rt", Region: mi.Region,
		AccessKey: mi.AccessKey, SecretKey: mi.SecretKey,
	}
	if err := backup.CreateBucket(ctx, dst, true); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	runner := &rtRunner{OneOffRunner: rtEngine(t)}
	svc := New(pool, runner, NewDBStore(db.New(pool)), dsn, true)
	label := rtLabel(t, svc)
	if err := svc.Save(ctx, Input{
		Endpoint: dst.Endpoint, Bucket: dst.Bucket, Region: dst.Region,
		AccessKey: dst.AccessKey, SecretKey: dst.SecretKey, Prefix: rtPrefix,
		Recipient: id.Recipient().String(), Preset: "off", Retention: 14,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := svc.SetEnabled(ctx, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	return &rtFixture{pool: pool, dsn: dsn, runner: runner, dst: dst, id: id, svc: svc, label: label}
}

// lastRun returns the newest journal row.
func (f *rtFixture) lastRun(t *testing.T) Run {
	t.Helper()
	runs, err := f.svc.Runs(context.Background(), 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, err = %v", runs, err)
	}
	return runs[0]
}

// stateObjects lists every object under <prefix>/krill-state/.
func (f *rtFixture) stateObjects(t *testing.T) []backup.Object {
	t.Helper()
	objs, err := backup.List(context.Background(), f.dst, rtPrefix+"/krill-state/", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return objs
}

// download returns the stored ciphertext of key.
func (f *rtFixture) download(t *testing.T, key string) []byte {
	t.Helper()
	rc, err := backup.Download(context.Background(), f.dst, key, true)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	return b
}

// rtOpen decrypts ciphertext with id and gunzips it.
func rtOpen(ciphertext []byte, id age.Identity) ([]byte, error) {
	ar, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(ar)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(gz)
}

func rtQueryInt(t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func rtMigration(t *testing.T, pool *pgxpool.Pool) (int64, bool) {
	t.Helper()
	var (
		v     int64
		dirty bool
	)
	if err := pool.QueryRow(context.Background(), "SELECT version, dirty FROM schema_migrations").Scan(&v, &dirty); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	return v, dirty
}

func TestStateBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newRTFixture(t, "krill")

	const sessionToken = "rt-live-session-token-7f3a"
	if _, err := f.pool.Exec(ctx, `
		WITH u AS (INSERT INTO users (email, password_hash) VALUES ('rt@krill.test', 'x') RETURNING id),
		     o AS (INSERT INTO organizations (name, slug, owner_id) SELECT 'RT', 'rt', id FROM u RETURNING id),
		     p AS (INSERT INTO projects (organization_id, name, slug) SELECT id, 'RT project', 'rt-project' FROM o RETURNING id)
		INSERT INTO sessions (token, user_id, expires_at) SELECT $1, id, now() + interval '1 day' FROM u`, sessionToken); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO metric_samples (component, cpu_pct, mem_bytes) VALUES ('rt-component', 1.5, 1024)`); err != nil {
		t.Fatalf("seed metric: %v", err)
	}
	srcVersion, srcDirty := rtMigration(t, f.pool)
	srcOrgs := rtQueryInt(t, f.pool, "SELECT count(*) FROM organizations")
	srcProjects := rtQueryInt(t, f.pool, "SELECT count(*) FROM projects")
	if srcOrgs == 0 || srcProjects == 0 {
		t.Fatalf("seed produced orgs=%d projects=%d", srcOrgs, srcProjects)
	}

	if err := f.svc.Run(ctx, TriggerManual); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := f.lastRun(t)
	if run.Status != "ok" || run.Error != "" {
		t.Fatalf("run = %+v", run)
	}
	if !strings.HasPrefix(run.ObjectKey, rtPrefix+"/krill-state/") || !strings.HasSuffix(run.ObjectKey, ".sql.gz.age") {
		t.Fatalf("object key = %q", run.ObjectKey)
	}
	if imgs := f.runner.used(); len(imgs) != 1 || imgs[0] != "postgres:16-alpine" {
		t.Fatalf("dump images = %v, want exactly one, the postgres 16 alpine image", imgs)
	}

	ciphertext := f.download(t, run.ObjectKey)
	if int64(len(ciphertext)) != run.SizeBytes {
		t.Fatalf("object holds %d bytes, journal says %d", len(ciphertext), run.SizeBytes)
	}
	plain, err := rtOpen(ciphertext, f.id)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Contains(plain, []byte("PostgreSQL database dump")) {
		t.Fatalf("plaintext is not a pg_dump script (%d bytes)", len(plain))
	}
	if bytes.Contains(plain, []byte(sessionToken)) {
		t.Fatal("the dump carries a live session token")
	}
	// The completion marker is held back: the archive is pg_dump's output.
	if bytes.Contains(plain, []byte("krill-state-backup-complete")) || !bytes.HasSuffix(plain, []byte("\n")) {
		t.Fatalf("the archive carries the completion marker or was cut: ...%q", plain[max(0, len(plain)-200):])
	}

	// Restore into a fresh, empty server.
	target, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("krill"), postgres.WithUsername("krill"), postgres.WithPassword("krill"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	testcontainers.CleanupContainer(t, target)
	if err != nil {
		t.Fatalf("start target postgres: %v", err)
	}
	if err := target.CopyToContainer(ctx, plain, "/tmp/state.sql", 0o600); err != nil {
		t.Fatalf("copy dump: %v", err)
	}
	code, out, err := target.Exec(ctx, []string{
		"psql", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-U", "krill", "-d", "krill", "-q", "-f", "/tmp/state.sql",
	}, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("psql exec: %v", err)
	}
	if code != 0 {
		msg, _ := io.ReadAll(out)
		t.Fatalf("psql exited %d: %s", code, msg)
	}
	tdsn, err := target.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := pgxpool.New(ctx, tdsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)

	if v, d := rtMigration(t, restored); v != srcVersion || d != srcDirty {
		t.Fatalf("restored schema_migrations = (%d, %v), source = (%d, %v)", v, d, srcVersion, srcDirty)
	}
	if n := rtQueryInt(t, restored, "SELECT count(*) FROM organizations"); n != srcOrgs {
		t.Fatalf("restored organizations = %d, source = %d", n, srcOrgs)
	}
	if n := rtQueryInt(t, restored, "SELECT count(*) FROM projects"); n != srcProjects {
		t.Fatalf("restored projects = %d, source = %d", n, srcProjects)
	}
	if n := rtQueryInt(t, restored, "SELECT count(*) FROM sessions"); n != 0 {
		t.Fatalf("restored sessions = %d, want 0", n)
	}
	if n := rtQueryInt(t, restored, "SELECT count(*) FROM metric_samples"); n != 0 {
		t.Fatalf("restored metric_samples = %d, want 0", n)
	}

	n, err := f.runner.RemoveContainersByLabel(ctx, docker.OneOffRoleLabel, f.label)
	if err != nil || n != 0 {
		t.Fatalf("leftover dump containers removed = %d, err = %v", n, err)
	}
}

func TestStateBackupWrongPassword(t *testing.T) {
	ctx := context.Background()
	const wrong = "wrong-pw-9c41"
	f := newRTFixture(t, wrong)

	err := f.svc.Run(ctx, TriggerManual)
	if err == nil {
		t.Fatal("Run with a wrong password succeeded")
	}
	if strings.Contains(err.Error(), wrong) {
		t.Fatalf("the error carries the password: %v", err)
	}
	if run := f.lastRun(t); run.Status != "error" || strings.Contains(run.Error, wrong) {
		t.Fatalf("run = %+v", run)
	}
	// The server checked the password it was given on stdin (none at all
	// would fail with "no password supplied").
	if !strings.Contains(err.Error(), "password authentication failed") {
		t.Fatalf("Run = %v, want a password authentication failure", err)
	}
	if objs := f.stateObjects(t); len(objs) != 0 {
		t.Fatalf("objects left after a failed dump: %v", objs)
	}
	if n, err := f.runner.RemoveContainersByLabel(ctx, docker.OneOffRoleLabel, f.label); err != nil || n != 0 {
		t.Fatalf("leftover dump containers removed = %d, err = %v", n, err)
	}
}

func TestStateBackupOtherIdentityCannotDecrypt(t *testing.T) {
	f := newRTFixture(t, "krill")
	if err := f.svc.Run(context.Background(), TriggerManual); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ciphertext := f.download(t, f.lastRun(t).ObjectKey)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rtOpen(ciphertext, other); err == nil {
		t.Fatal("another identity decrypted the backup")
	}
	if _, err := rtOpen(ciphertext, f.id); err != nil {
		t.Fatalf("the recipient's own identity failed: %v", err)
	}
}

// rtTLSFiles writes a test CA and a server certificate for localhost to a
// directory under $HOME (shared into Colima's VM, so it can be bind-mounted).
func rtTLSFiles(t *testing.T) (dir, caFile, certFile, keyFile string) {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("HOME"), "krill-sbk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "krill state backup test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatal(err)
	}

	write := func(name, typ string, der []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return dir,
		write("ca.pem", "CERTIFICATE", caDER, 0o644),
		write("server.crt", "CERTIFICATE", srvDER, 0o644),
		write("server.key", "EC PRIVATE KEY", srvKeyDER, 0o600)
}

func TestStateBackupVerifyFullCABundle(t *testing.T) {
	ctx := context.Background()
	dir, caFile, certFile, keyFile := rtTLSFiles(t)

	// The key must belong to the postgres user before the server reads it.
	const prep = `cp /tls/server.crt /tls/server.key /var/lib/postgresql/ && ` +
		`chown postgres:postgres /var/lib/postgresql/server.crt /var/lib/postgresql/server.key && ` +
		`chmod 0600 /var/lib/postgresql/server.key && exec docker-entrypoint.sh "$@"`
	src, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("krill"), postgres.WithUsername("krill"), postgres.WithPassword("krill"),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{HostFilePath: certFile, ContainerFilePath: "/tls/server.crt", FileMode: 0o644},
			testcontainers.ContainerFile{HostFilePath: keyFile, ContainerFilePath: "/tls/server.key", FileMode: 0o600},
		),
		testcontainers.WithEntrypoint("sh", "-c", prep, "sh"),
		testcontainers.WithCmd("postgres", "-c", "ssl=on",
			"-c", "ssl_cert_file=/var/lib/postgresql/server.crt",
			"-c", "ssl_key_file=/var/lib/postgresql/server.key"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	testcontainers.CleanupContainer(t, src)
	if err != nil {
		t.Fatalf("start TLS postgres: %v", err)
	}
	raw, err := src.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Krill's own pool: encrypted, the dump's version query goes through it.
	srcPool, err := pgxpool.New(ctx, rtLocalDSN(t, raw, "krill", "sslmode=require"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srcPool.Close)
	if _, err := srcPool.Exec(ctx, `CREATE TABLE tls_marker (v TEXT); INSERT INTO tls_marker VALUES ('over-tls')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var ssl bool
	if err := srcPool.QueryRow(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&ssl); err != nil || !ssl {
		t.Fatalf("the server does not speak TLS: ssl=%v err=%v", ssl, err)
	}

	// The settings live in a separate, migrated database.
	storePool := testutil.NewTestDB(t)
	mi := testutil.NewMinio(t)
	dst := backup.Destination{
		Endpoint: mi.Endpoint, Bucket: "krill-state-tls", Region: mi.Region,
		AccessKey: mi.AccessKey, SecretKey: mi.SecretKey,
	}
	if err := backup.CreateBucket(ctx, dst, true); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	// verify-full with no sslrootcert: the CA comes from the bundle list.
	dsn := rtLocalDSN(t, raw, "krill", "sslmode=verify-full")
	runner := &rtRunner{OneOffRunner: rtEngine(t)}
	svc := New(srcPool, runner, NewDBStore(db.New(storePool)), dsn, true)
	svc.caBundles = []string{filepath.Join(dir, "missing-bundle.pem"), caFile}
	label := rtLabel(t, svc)
	if err := svc.Save(ctx, Input{
		Endpoint: dst.Endpoint, Bucket: dst.Bucket, Region: dst.Region,
		AccessKey: dst.AccessKey, SecretKey: dst.SecretKey, Prefix: rtPrefix,
		Recipient: id.Recipient().String(), Preset: "off", Retention: 14,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := svc.SetEnabled(ctx, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	// The dump verifies the certificate itself: with a bundle from another
	// CA it must fail, even though libpq's default (prefer) would connect
	// and the pool stays connected.
	_, otherCA, _, _ := rtTLSFiles(t)
	svc.caBundles = []string{otherCA}
	if err := svc.Run(ctx, TriggerManual); err == nil || !strings.Contains(err.Error(), "certificate verify failed") {
		t.Fatalf("a dump trusting another CA = %v; want a certificate verification failure", err)
	}
	if objs, err := backup.List(ctx, dst, rtPrefix+"/", true); err != nil || len(objs) != 0 {
		t.Fatalf("objects after the refused dump = %+v, err = %v", objs, err)
	}
	svc.caBundles = []string{filepath.Join(dir, "missing-bundle.pem"), caFile}

	if err := svc.Run(ctx, TriggerManual); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runs, err := svc.Runs(ctx, 1)
	if err != nil || len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	rc, err := backup.Download(ctx, dst, runs[0].ObjectKey, true)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	ciphertext, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rtOpen(ciphertext, id)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Contains(plain, []byte("over-tls")) {
		t.Fatal("the dump lacks the marker row")
	}
	if n, err := runner.RemoveContainersByLabel(ctx, docker.OneOffRoleLabel, label); err != nil || n != 0 {
		t.Fatalf("leftover dump containers removed = %d, err = %v", n, err)
	}
}
