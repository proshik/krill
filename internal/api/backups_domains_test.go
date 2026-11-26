package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
)

// fakeBackups records Start calls and answers Running from a set.
type fakeBackups struct {
	mu      sync.Mutex
	started []string
	running map[string]bool
	err     error
}

func (b *fakeBackups) Start(kind string, id int64, _ time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.started = append(b.started, kind)
	return nil
}

func (b *fakeBackups) Running(kind string, _ int64) bool { return b.running[kind] }

// backupFixture adds one backup of each kind to the acme fixture: a pg_dump of
// logical database "app" (linked to bot), a volume backup of bot's "data", and
// a bucket backup of bucket "files" (not linked).
type backupFixture struct {
	*apiFixture
	envID                             int64
	dbBackup, volBackup, bucketBackup int64
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	f := newAPIFixture(t)
	ctx := t.Context()
	app, err := f.q.GetApplication(ctx, f.appID)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := f.q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: f.ident.OrgID, Name: "offsite", Bucket: "bk", Region: "us-east-1", AccessKey: "AKIDEXAMPLE", SecretKey: "dest-secret",
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	pg, err := f.q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: f.ident.OrgID, Engine: "postgres", Name: "main", AppName: "krill-postgres-main-test", Image: "postgres:16", Superuser: "postgres", SuperuserPassword: "root-secret",
	})
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	ldb, err := f.q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: pg.ID, EnvironmentID: app.EnvironmentID, Name: "app", DbName: "app", Username: "app", Password: "db-secret",
	})
	if err != nil {
		t.Fatalf("logical db: %v", err)
	}
	if _, err := f.q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, LogicalDatabaseID: &ldb.ID, VarName: "DATABASE_URL", Scheme: "postgres", Field: "url"}); err != nil {
		t.Fatalf("link: %v", err)
	}
	b, err := f.q.CreateBackup(ctx, db.CreateBackupParams{LogicalDatabaseID: ldb.ID, DestinationID: dest.ID, Schedule: "0 3 * * *", Retention: 7, Enabled: true})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	vol, err := f.q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"})
	if err != nil {
		t.Fatal(err)
	}
	vb, err := f.q.CreateVolumeBackup(ctx, db.CreateVolumeBackupParams{AppVolumeID: vol.ID, DestinationID: dest.ID, Schedule: "@daily", Retention: 3, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	minio, err := f.q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: f.ident.OrgID, Engine: "minio", Name: "files", AppName: "krill-minio-files-test", Image: "minio/minio", Superuser: "root", SuperuserPassword: "minio-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	bk, err := f.q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: minio.ID, EnvironmentID: app.EnvironmentID, Name: "uploads", AccessKey: "ak", SecretKey: "bucket-secret", CreatedByKrill: true})
	if err != nil {
		t.Fatal(err)
	}
	bb, err := f.q.CreateBucketBackup(ctx, db.CreateBucketBackupParams{BucketID: bk.ID, DestinationID: dest.ID, Schedule: "@weekly", Retention: 2})
	if err != nil {
		t.Fatal(err)
	}
	return &backupFixture{apiFixture: f, envID: app.EnvironmentID, dbBackup: b.ID, volBackup: vb.ID, bucketBackup: bb.ID}
}

func TestListBackupsCoversEveryKindAndFilters(t *testing.T) {
	f := newBackupFixture(t)
	ctx := t.Context()
	// The last run failed with credentials in its text; an older run was ok.
	ranAt := time.Now().Add(-time.Hour)
	if err := f.q.SetBackupResult(ctx, db.SetBackupResultParams{
		ID: f.dbBackup, LastRunAt: pgtype.Timestamptz{Time: ranAt, Valid: true}, LastStatus: "error",
		LastError: "upload: PUT https://AKIDEXAMPLE:dest-secret@s3.example.test/bk?X-Amz-Signature=deadbeef failed: password=db-secret",
	}); err != nil {
		t.Fatal(err)
	}
	okAt := time.Now().Add(-26 * time.Hour).UTC().Truncate(time.Second)
	if err := f.q.InsertBackupRun(ctx, db.InsertBackupRunParams{OrgID: f.ident.OrgID, Kind: "database", SourceID: f.dbBackup, Status: "ok", FinishedAt: okAt}); err != nil {
		t.Fatal(err)
	}
	svc := f.svc
	bk := &fakeBackups{running: map[string]bool{"volume": true}}
	svc.SetBackups(bk)

	all, err := svc.ListBackups(ctx, f.readIdent, api.BackupFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want one backup of each kind, got %+v", all)
	}
	byKind := map[string]api.BackupInfo{}
	for _, b := range all {
		byKind[b.Kind] = b
	}
	d := byKind["database"]
	if d.Target != "acme-proj/production/app" || d.Server != "main" || d.Destination != "offsite" || d.LastStatus != "error" || d.LastRunAt == "" {
		t.Fatalf("database backup: %+v", d)
	}
	if d.LastSuccessAt != okAt.Format(time.RFC3339) {
		t.Fatalf("last success from the run history: %q, want %q", d.LastSuccessAt, okAt.Format(time.RFC3339))
	}
	for _, leak := range []string{"dest-secret", "deadbeef", "db-secret"} {
		if strings.Contains(d.LastError, leak) {
			t.Fatalf("last_error leaks %q: %s", leak, d.LastError)
		}
	}
	if v := byKind["volume"]; v.Target != "acme-proj/production/bot:data" || !v.Running || v.Enabled {
		t.Fatalf("volume backup: %+v", v)
	}
	if b := byKind["bucket"]; b.Target != "acme-proj/production/uploads" || b.Server != "files" || b.Running {
		t.Fatalf("bucket backup: %+v", b)
	}
	if strings.Contains(mustJSON(t, all), "secret") && !strings.Contains(mustJSON(t, all), "***") {
		t.Fatalf("list leaks a secret: %s", mustJSON(t, all))
	}

	// bot has the volume and is linked to the database, not to the bucket.
	forApp, err := svc.ListBackups(ctx, f.readIdent, api.BackupFilter{App: "acme-proj/production/bot"})
	if err != nil || len(forApp) != 2 {
		t.Fatalf("app filter: %+v %v", forApp, err)
	}
	forEnv, err := svc.ListBackups(ctx, f.readIdent, api.BackupFilter{Environment: "acme-proj/production"})
	if err != nil || len(forEnv) != 3 {
		t.Fatalf("environment filter: %+v %v", forEnv, err)
	}
	// Another organization sees none of them, and cannot name the app.
	other, err := svc.ListBackups(ctx, f.otherIdent, api.BackupFilter{})
	if err != nil || len(other) != 0 {
		t.Fatalf("foreign list: %+v %v", other, err)
	}
	var aerr *api.Error
	if _, err := svc.ListBackups(ctx, f.otherIdent, api.BackupFilter{App: f.appIDString}); !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("foreign app filter: %v", err)
	}
}

func TestBackupNowGatesAndStarts(t *testing.T) {
	f := newBackupFixture(t)
	ctx := t.Context()
	bk := &fakeBackups{}
	f.svc.SetBackups(bk)
	var aerr *api.Error

	if _, err := f.svc.BackupNow(ctx, f.readIdent, api.BackupDatabase, f.dbBackup); !errors.As(err, &aerr) || aerr.Code != api.CodeForbidden {
		t.Fatalf("read token: %v", err)
	}
	demoted := f.ident
	demoted.Role = auth.RoleMember
	if _, err := f.svc.BackupNow(ctx, demoted, api.BackupDatabase, f.dbBackup); !errors.As(err, &aerr) || aerr.Code != api.CodeForbidden {
		t.Fatalf("a write token of a demoted member: %v", err)
	}
	for _, kind := range []string{api.BackupDatabase, api.BackupVolume, api.BackupBucket} {
		id := map[string]int64{api.BackupDatabase: f.dbBackup, api.BackupVolume: f.volBackup, api.BackupBucket: f.bucketBackup}[kind]
		if _, err := f.svc.BackupNow(ctx, f.otherIdent, kind, id); !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
			t.Fatalf("%s of another org: %v", kind, err)
		}
		res, err := f.svc.BackupNow(ctx, f.ident, kind, id)
		if err != nil || res.Status != "started" || res.StartedAt == "" || res.Kind != kind {
			t.Fatalf("%s: %+v %v", kind, res, err)
		}
	}
	if len(bk.started) != 3 {
		t.Fatalf("started %v", bk.started)
	}
	if _, err := f.svc.BackupNow(ctx, f.ident, "state", 1); !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
		t.Fatalf("unknown kind: %v", err)
	}
	bk.err = api.ErrBackupBusy
	if _, err := f.svc.BackupNow(ctx, f.ident, api.BackupDatabase, f.dbBackup); !errors.As(err, &aerr) || aerr.Code != api.CodeConflict {
		t.Fatalf("busy: %v", err)
	}
}

func TestRedactBackupError(t *testing.T) {
	cases := map[string]string{
		"dial tcp 10.0.0.1:9000: connection refused":                                      "dial tcp 10.0.0.1:9000: connection refused",
		"GET https://user:pa55@host/x":                                                    "GET https://***@host/x",
		"api error SignatureDoesNotMatch; X-Amz-Credential=AKID/2026 X-Amz-Signature=abc": "api error SignatureDoesNotMatch; X-Amz-Credential=*** X-Amz-Signature=***",
		`psql: password="s3cr et" rejected`:                                               `psql: password=*** rejected`,
		"token: abc123":                                                                   "token: ***",
	}
	for in, want := range cases {
		if got := api.RedactBackupError(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("ж", 400)
	if got := api.RedactBackupError(long); len(got) > 504 || !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Errorf("long error not cut cleanly: %d bytes", len(got))
	}
}

// fakeGateway knows its addresses and where to dial.
type fakeGateway struct{ addrs []string }

func (g fakeGateway) Addresses(context.Context) ([]string, error) { return g.addrs, nil }
func (fakeGateway) DialAddr() string                              { return "gateway.internal" }

func selfSigned(t *testing.T, cn string, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, Issuer: pkix.Name{CommonName: cn},
		NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter, DNSNames: []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDomainStatusChecksDNSAndCertificate(t *testing.T) {
	f := newAPIFixture(t)
	ctx := t.Context()
	if _, err := f.q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: f.appID, Host: "hook.example.test", Tls: true, Exposed: true}); err != nil {
		t.Fatal(err)
	}
	// TLS on but not exposed: it can still carry an HTTPS redirect router.
	if _, err := f.q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: f.appID, Host: "old.example.test", Tls: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: f.appID, Host: "plain.example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: f.appID, Host: "down.example.test", Tls: true, Exposed: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	issued := selfSigned(t, "hook.example.test", now.Add(30*24*time.Hour))
	roots := x509.NewCertPool()
	roots.AddCert(issued)
	defaultCert := selfSigned(t, "TRAEFIK DEFAULT CERT", now.Add(365*24*time.Hour))

	var mu sync.Mutex
	var dialed []string
	f.svc.SetGateway(fakeGateway{addrs: []string{"203.0.113.10"}})
	f.svc.SetDomainProbe(
		func(_ context.Context, host string) ([]string, error) {
			switch host {
			case "hook.example.test":
				return []string{"203.0.113.10"}, nil
			case "old.example.test":
				return []string{"198.51.100.7"}, nil
			}
			return nil, errors.New("no such host")
		},
		func(_ context.Context, addr, sni string) ([]*x509.Certificate, error) {
			mu.Lock()
			dialed = append(dialed, addr+"|"+sni)
			mu.Unlock()
			switch sni {
			case "hook.example.test":
				return []*x509.Certificate{issued}, nil
			case "down.example.test":
				return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.9.9.9"), Port: 443}, Err: errors.New("connect: connection refused")}
			}
			return []*x509.Certificate{defaultCert}, nil
		},
		roots, func() time.Time { return now },
	)
	res, err := f.svc.DomainStatus(ctx, f.readIdent, f.appIDString)
	if err != nil {
		t.Fatalf("domain status: %v", err)
	}
	got := map[string]api.DomainCheck{}
	for _, d := range res.Domains {
		got[d.Host] = d
	}
	hook, old, plain := got["hook.example.test"], got["old.example.test"], got["plain.example.test"]
	if hook.PointsAtGateway == nil || !*hook.PointsAtGateway || hook.Certificate == nil || !hook.Certificate.Trusted || hook.Certificate.DefaultCert || hook.Certificate.DaysLeft != 30 {
		t.Fatalf("hook: %+v %+v", hook, hook.Certificate)
	}
	if old.PointsAtGateway == nil || *old.PointsAtGateway || old.Certificate == nil || !old.Certificate.DefaultCert || old.Certificate.Trusted {
		t.Fatalf("old: %+v %+v", old, old.Certificate)
	}
	if plain.DNS.Error == "" || plain.PointsAtGateway != nil || plain.Certificate != nil {
		t.Fatalf("plain (no TLS, unresolvable): %+v", plain)
	}
	// Only the gateway's own address is ever dialed, with the host as SNI.
	for _, d := range dialed {
		if !strings.HasPrefix(d, "gateway.internal|") {
			t.Fatalf("dialed %s", d)
		}
	}
	if len(dialed) != 3 {
		t.Fatalf("dialed %v", dialed)
	}

	if !res.GatewayKnown || strings.Contains(mustJSON(t, res), "gateway_addresses") {
		t.Fatalf("the gateway's addresses are operator data and must not be listed: %s", mustJSON(t, res))
	}
	down := got["down.example.test"]
	if down.Certificate == nil || down.Certificate.Error == "" || strings.Contains(down.Certificate.Error, "10.9.9.9") {
		t.Fatalf("a failed handshake must not name the gateway's address: %+v", down.Certificate)
	}

	// Gateway addresses unknown: only the resolution is reported.
	f.svc.SetGateway(fakeGateway{})
	res, err = f.svc.DomainStatus(ctx, f.readIdent, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Domains {
		if d.PointsAtGateway != nil {
			t.Fatalf("unknown gateway addresses must not answer points_at_gateway: %+v", d)
		}
	}
	if _, err := f.svc.DomainStatus(ctx, f.otherIdent, f.appIDString); err == nil {
		t.Fatal("foreign app must be not found")
	}
}
