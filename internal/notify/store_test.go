package notify_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

func TestChannelsForOrgDecryptsToken(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")

	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	ctx := context.Background()

	ownerID := mkUser(t, q, "n-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")

	stored, err := q.UpsertNotificationChannel(ctx, db.UpsertNotificationChannelParams{
		OrgID: o.ID, Type: "telegram", Enabled: true,
		BotToken: secret.Enc("super-token"), ChatID: "42",
		NotifyDeploy: true, NotifyBackup: true, NotifyHealth: true,
		DiskThresholdPct: 85, MemThresholdPct: 100, DigestMinute: 660, DigestTz: "Europe/Moscow", Lang: "en",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !strings.HasPrefix(stored.BotToken, "enc:v1:") {
		t.Fatalf("token not encrypted at rest: %q", stored.BotToken)
	}

	st := notify.NewDBStore(q)
	chans, err := st.ChannelsForOrg(ctx, o.ID)
	if err != nil || len(chans) != 1 {
		t.Fatalf("ChannelsForOrg: %v, n=%d", err, len(chans))
	}
	if chans[0].BotToken != "super-token" {
		t.Fatalf("token not decrypted: %q", chans[0].BotToken)
	}
}

// A bot token that cannot be decrypted (key rotated or KRILL_SECRET_KEY unset)
// must surface as an error. Handing back the channel with an empty token sends
// every alert to Telegram with no credential: the send fails with an opaque API
// error, nothing points at the key, and alerts silently stop arriving.
func TestChannelsForOrgFailsOnUndecryptableToken(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	ctx := context.Background()

	ownerID := mkUser(t, q, "n-rot@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")

	secret.Init("old-key")
	encTok := secret.Enc("super-token")
	secret.Init("new-key")
	defer secret.Init("")

	if _, err := q.UpsertNotificationChannel(ctx, db.UpsertNotificationChannelParams{
		OrgID: o.ID, Type: "telegram", Enabled: true, BotToken: encTok, ChatID: "42",
		NotifyDeploy: true, NotifyBackup: true, NotifyHealth: true,
		DiskThresholdPct: 85, MemThresholdPct: 100, DigestMinute: 660, DigestTz: "Europe/Moscow", Lang: "en",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	chans, err := notify.NewDBStore(q).ChannelsForOrg(ctx, o.ID)
	if err == nil {
		t.Fatalf("expected an error for an undecryptable token, got %+v", chans)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
	}
}

// TestBackupTargetResolvesThroughLogicalDatabase covers the migration-000030
// rewrite of BackupNotifyTarget: it now joins backups -> logical_databases ->
// db_instances (the legacy postgres_dbs join is gone), and must still resolve
// the org/project/env plus a display name for the failure-alert message.
func TestBackupTargetResolvesThroughLogicalDatabase(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	ctx := context.Background()

	ownerID := mkUser(t, q, "n-backup-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "prod")

	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "maindb", AppName: "krill-postgres-maindb-nt1",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "secret",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "maindb", DbName: "app", Username: "postgres", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create logical database: %v", err)
	}
	dest, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "s3", Endpoint: "http://m:9000", Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	b, err := q.CreateBackup(ctx, db.CreateBackupParams{
		LogicalDatabaseID: ldb.ID, DestinationID: dest.ID, Schedule: "0 3 * * *", Retention: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}

	st := notify.NewDBStore(q)
	tgt, err := st.BackupTarget(ctx, b.ID)
	if err != nil {
		t.Fatalf("BackupTarget: %v", err)
	}
	if tgt.OrgID != o.ID || tgt.Project != "P" || tgt.Env != "prod" || tgt.Name != "krill-postgres-maindb-nt1" {
		t.Fatalf("target = %+v", tgt)
	}
}

func mkUser(t *testing.T, q *db.Queries, email string) int64 {
	t.Helper()
	u, err := q.CreateUser(context.Background(), db.CreateUserParams{Email: email, PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}

// Each kind of backup resolves its owner through its own table. Their ids are
// independent sequences, so in a fresh database the first DB, volume and
// bucket backups all have id 1 — here in three different organizations, and a
// lookup through the wrong table would alert the wrong one.
func TestBackupTargetsResolveThroughTheirOwnTables(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	ctx := context.Background()
	mk := func(name string) (int64, int64, int64) {
		uid := mkUser(t, q, name+"@k.local")
		o, _ := orgSvc.CreateOrg(ctx, uid, name)
		p, _ := orgSvc.CreateProject(ctx, o.ID, "P-"+name, "")
		e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "prod")
		d, err := q.CreateDestination(ctx, db.CreateDestinationParams{OrganizationID: o.ID, Name: "s3", Endpoint: "http://m:9000", Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s"})
		if err != nil {
			t.Fatal(err)
		}
		return o.ID, e.ID, d.ID
	}
	dbOrg, dbEnv, dbDest := mk("dborg")
	inst, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: dbOrg, Engine: "postgres", Name: "pg", AppName: "krill-postgres-pg-nt2", Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x"})
	ldb, _ := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: inst.ID, EnvironmentID: dbEnv, Name: "app", DbName: "app", Username: "app", Password: "x"})
	dbb, err := q.CreateBackup(ctx, db.CreateBackupParams{LogicalDatabaseID: ldb.ID, DestinationID: dbDest, Schedule: "0 3 * * *", Retention: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	volOrg, volEnv, volDest := mk("volorg")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: volEnv, Name: "web", Image: "nginx", Tag: "1", Domain: "w-nt.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	vol, _ := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"})
	vb, err := q.CreateVolumeBackup(ctx, db.CreateVolumeBackupParams{AppVolumeID: vol.ID, DestinationID: volDest, Schedule: "0 3 * * *", Retention: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	bktOrg, bktEnv, bktDest := mk("bktorg")
	s3, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: bktOrg, Engine: "minio", Name: "silo", AppName: "krill-minio-silo-nt3", Image: "x", Superuser: "r", SuperuserPassword: "x"})
	bucket, _ := q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: s3.ID, EnvironmentID: bktEnv, Name: "assets", AccessKey: "k", SecretKey: "x", CreatedByKrill: true})
	bb, err := q.CreateBucketBackup(ctx, db.CreateBucketBackupParams{BucketID: bucket.ID, DestinationID: bktDest, Schedule: "0 3 * * *", Retention: 7})
	if err != nil {
		t.Fatal(err)
	}
	if dbb.ID != vb.ID || vb.ID != bb.ID {
		t.Fatalf("setup: the ids should coincide (%d %d %d)", dbb.ID, vb.ID, bb.ID)
	}

	st := notify.NewDBStore(q)
	if tgt, err := st.BackupTarget(ctx, dbb.ID); err != nil || tgt.OrgID != dbOrg {
		t.Fatalf("db backup: %+v %v", tgt, err)
	}
	if tgt, err := st.VolumeBackupTarget(ctx, vb.ID); err != nil || tgt.OrgID != volOrg || tgt.Name != "web / data" {
		t.Fatalf("volume backup: %+v %v", tgt, err)
	}
	if tgt, err := st.BucketBackupTarget(ctx, bb.ID); err != nil || tgt.OrgID != bktOrg || tgt.Name != "assets" {
		t.Fatalf("bucket backup: %+v %v", tgt, err)
	}
}

func chanParams(orgID int64, token string) db.UpsertNotificationChannelParams {
	return db.UpsertNotificationChannelParams{
		OrgID: orgID, Type: "telegram", Enabled: true, BotToken: token, ChatID: "42",
		NotifyDeploy: true, NotifyBackup: true, NotifyHealth: true,
		DiskThresholdPct: 85, MemThresholdPct: 100, DigestMinute: 660, DigestTz: "Europe/Moscow", Lang: "en",
	}
}

// The new channel settings survive a round trip, and the cross-organization
// lists return only what their flags select.
func TestInfraAndDigestChannels(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	ctx := context.Background()
	mk := func(name string, mut func(*db.UpsertNotificationChannelParams)) int64 {
		o, _ := orgSvc.CreateOrg(ctx, mkUser(t, q, name+"@k.local"), name)
		p := chanParams(o.ID, "tok-"+name)
		mut(&p)
		if _, err := q.UpsertNotificationChannel(ctx, p); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return o.ID
	}
	infraOrg := mk("infra", func(p *db.UpsertNotificationChannelParams) {
		p.NotifyInfra, p.DiskThresholdPct, p.MemThresholdPct, p.Lang = true, 90, 120, "ru"
	})
	digestOrg := mk("digest", func(p *db.UpsertNotificationChannelParams) {
		p.DigestEnabled, p.DigestMinute, p.DigestTz, p.NotifyDeploySuccess = true, 9*60+30, "Asia/Tbilisi", true
	})
	mk("off", func(p *db.UpsertNotificationChannelParams) {
		p.Enabled, p.NotifyInfra, p.DigestEnabled = false, true, true
	})

	st := notify.NewDBStore(q)
	infra, err := st.InfraChannels(ctx)
	if err != nil || len(infra) != 1 || infra[0].OrgID != infraOrg {
		t.Fatalf("InfraChannels = %+v, %v", infra, err)
	}
	if c := infra[0]; c.DiskThresholdPct != 90 || c.MemThresholdPct != 120 || c.Lang != "ru" || c.BotToken != "tok-infra" || c.ID == 0 {
		t.Fatalf("infra channel fields = %+v", c)
	}
	dg, err := st.DigestChannels(ctx)
	if err != nil || len(dg) != 1 || dg[0].OrgID != digestOrg {
		t.Fatalf("DigestChannels = %+v, %v", dg, err)
	}
	if c := dg[0]; c.DigestMinute != 570 || c.DigestTZ != "Asia/Tbilisi" || !c.NotifyDeploySuccess || c.DigestLastAt != nil {
		t.Fatalf("digest channel fields = %+v", c)
	}

	// One organization's broken token must not silence the others.
	secret.Init("k1")
	defer secret.Init("")
	o, _ := orgSvc.CreateOrg(ctx, mkUser(t, q, "broken@k.local"), "broken")
	p := chanParams(o.ID, secret.Enc("x"))
	p.NotifyInfra = true
	if _, err := q.UpsertNotificationChannel(ctx, p); err != nil {
		t.Fatal(err)
	}
	secret.Init("k2")
	infra, err = st.InfraChannels(ctx)
	if err == nil || len(infra) != 1 || infra[0].OrgID != infraOrg {
		t.Fatalf("InfraChannels with a broken token = %+v, %v; want the good one plus an error", infra, err)
	}
}

func TestClaimAndReleaseDigest(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	o, _ := org.NewService(q).CreateOrg(ctx, mkUser(t, q, "c@k.local"), "Org")
	ch, err := q.UpsertNotificationChannel(ctx, chanParams(o.ID, "t"))
	if err != nil {
		t.Fatal(err)
	}
	st := notify.NewDBStore(q)
	sched := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	at := sched.Add(30 * time.Second)

	if ok, err := st.ClaimDigest(ctx, ch.ID, at, sched); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if ok, _ := st.ClaimDigest(ctx, ch.ID, at.Add(time.Minute), sched); ok {
		t.Fatal("the same occurrence was claimed twice")
	}
	if err := st.ReleaseDigest(ctx, ch.ID, at, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ClaimDigest(ctx, ch.ID, at.Add(2*time.Minute), sched); !ok {
		t.Fatal("not claimable after a release")
	}
	// A release with a stale claim time leaves the newer claim alone.
	if err := st.ReleaseDigest(ctx, ch.ID, at, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ClaimDigest(ctx, ch.ID, at.Add(3*time.Minute), sched); ok {
		t.Fatal("a stale release undid a newer claim")
	}
	// The next day's occurrence is claimable.
	if ok, _ := st.ClaimDigest(ctx, ch.ID, at.Add(24*time.Hour), sched.Add(24*time.Hour)); !ok {
		t.Fatal("next day not claimable")
	}
}

func TestAlertStateAndBackupJournal(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	orgSvc := org.NewService(q)
	o, _ := orgSvc.CreateOrg(ctx, mkUser(t, q, "a@k.local"), "Org")
	other, _ := orgSvc.CreateOrg(ctx, mkUser(t, q, "b@k.local"), "Other")
	ch, _ := q.UpsertNotificationChannel(ctx, chanParams(o.ID, "t"))
	st := notify.NewDBStore(q)

	if s, err := st.AlertState(ctx, ch.ID, "disk:cp:/"); err != nil || s.Active || s.LastSent != nil {
		t.Fatalf("missing state = %+v, %v", s, err)
	}
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	if err := st.SetAlertState(ctx, ch.ID, "disk:cp:/", notify.AlertState{Active: true, LastSent: &now}); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.AlertState(ctx, ch.ID, "disk:cp:/"); !s.Active || s.LastSent == nil || !s.LastSent.Equal(now) {
		t.Fatalf("stored state = %+v", s)
	}

	for _, r := range []notify.BackupRun{
		{OrgID: o.ID, Kind: notify.BackupKindDatabase, SourceID: 1, Target: "old", Status: "ok", FinishedAt: now.Add(-48 * time.Hour)},
		{OrgID: o.ID, Kind: notify.BackupKindVolume, SourceID: 1, Project: "p", Env: "e", Target: "web / data", Status: "ok", SizeBytes: 2048, FinishedAt: now},
		{OrgID: o.ID, Kind: notify.BackupKindBucket, SourceID: 1, Target: "assets", Status: "error", Error: "denied", FinishedAt: now},
		{OrgID: other.ID, Kind: notify.BackupKindDatabase, SourceID: 1, Target: "theirs", Status: "ok", FinishedAt: now},
	} {
		if err := st.RecordBackupRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := st.BackupRunsSince(ctx, o.ID, now.Add(-24*time.Hour))
	if err != nil || len(runs) != 2 || runs[0].SizeBytes != 2048 && runs[1].SizeBytes != 2048 {
		t.Fatalf("BackupRunsSince = %+v, %v", runs, err)
	}
	if err := st.PruneBackupRuns(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if runs, _ := st.BackupRunsSince(ctx, o.ID, time.Time{}); len(runs) != 2 {
		t.Fatalf("after prune = %+v", runs)
	}
}

func TestDeploymentQueries(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	orgSvc := org.NewService(q)
	o, _ := orgSvc.CreateOrg(ctx, mkUser(t, q, "d@k.local"), "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "prod")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "1", Domain: "w-dq.x", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"})
	if err := q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: done.ID, Status: "done", ImageTag: "nginx:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app.ID, Trigger: "manual"}); err != nil { // still running
		t.Fatal(err)
	}
	st := notify.NewDBStore(q)
	info, err := st.DeploymentInfo(ctx, done.ID)
	if err != nil || info.OrgID != o.ID || info.App != "web" || info.Project != "P" || info.ImageTag != "nginx:1" || info.FinishedAt.IsZero() || info.Trigger != "manual" {
		t.Fatalf("DeploymentInfo = %+v, %v", info, err)
	}
	deps, err := st.OrgDeploymentsSince(ctx, o.ID, time.Now().Add(-time.Hour))
	if err != nil || len(deps) != 1 || deps[0].ID != done.ID {
		t.Fatalf("OrgDeploymentsSince = %+v, %v (an unfinished deploy must not be listed)", deps, err)
	}
	apps, err := st.OrgApps(ctx, o.ID)
	if err != nil || len(apps) != 1 || apps[0].AppID != app.ID || apps[0].Env != "prod" {
		t.Fatalf("OrgApps = %+v, %v", apps, err)
	}
}

func TestListWatchedDBsCoversEveryOrganization(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	ctx := context.Background()
	var orgs []int64
	for _, name := range []string{"wa", "wb"} {
		o, err := orgSvc.CreateOrg(ctx, mkUser(t, q, name+"@k.local"), name)
		if err != nil {
			t.Fatal(err)
		}
		orgs = append(orgs, o.ID)
	}
	if _, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: orgs[0], Engine: "postgres", Name: "pg", AppName: "krill-postgres-pg-w1", Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x", NodeHostname: "node-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: orgs[1], Engine: "redis", Name: "cache", AppName: "krill-redis-cache-w2", Image: "redis:7", Superuser: "default", SuperuserPassword: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := notify.NewDBStore(q).ListWatchedDBs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want both organizations' servers, got %+v", got)
	}
	byService := map[string]notify.WatchedDB{}
	for _, w := range got {
		byService[w.Service] = w
	}
	pg := byService["krill-postgres-pg-w1"]
	if pg.OrgID != orgs[0] || pg.Name != "pg" || pg.Node != "node-1" || pg.Status != "idle" || pg.InstanceID == 0 {
		t.Fatalf("postgres row = %+v", pg)
	}
	if byService["krill-redis-cache-w2"].OrgID != orgs[1] {
		t.Fatalf("redis row = %+v", byService["krill-redis-cache-w2"])
	}
}
