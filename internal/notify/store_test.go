package notify_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
