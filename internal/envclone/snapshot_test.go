package envclone

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/proshik/krill/internal/dbservice"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

func TestSnapshotSanitizesBeforeReturningAndKeepsCapturedValues(t *testing.T) {
	ctx := t.Context()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "snapshot@fixture.test", PasswordHash: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	os := org.NewService(q)
	o, err := os.CreateOrg(ctx, u.ID, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.CreateProject(ctx, o.ID, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := os.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Name: "pg", Engine: "postgres", AppName: "fixture-pg", Image: "postgres:16-alpine", Superuser: "root", SuperuserPassword: "SOURCE_SUPERUSER_SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	ld, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: inst.ID, EnvironmentID: e.ID, Name: "data", DbName: "source_db", Username: "source_user", Password: "SOURCE_DB_SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: e.ID, Name: "web", Image: "http-app", Tag: "stable", Domain: "source.test", SourceType: "image", Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: app.ID, EnvText: "# keep\nPUBLIC=https://source.test\nSECRET=SOURCE_ENV_SECRET\nDB=SOURCE_LITERAL_DSN", EnvPublicKeys: []string{"PUBLIC", "DB"}}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationBuild(ctx, db.UpdateApplicationBuildParams{ID: app.ID, BuildArgs: "PUBLIC=1", BuildSecrets: "SOURCE_BUILD_SECRET"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, LogicalDatabaseID: &ld.ID, VarName: "DB", Scheme: "postgresql", Field: "url"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "files", MountPath: "/files"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: app.ID, Port: 8080, Path: "/metrics", Job: "web"}); err != nil {
		t.Fatal(err)
	}
	snap, err := ReadSnapshot(ctx, pool, o.ID, p.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SOURCE_") {
		t.Fatal("snapshot contains source secret data")
	}
	if len(snap.Apps) != 1 || len(snap.Databases) != 1 || len(snap.Apps[0].Links) != 1 || len(snap.Apps[0].Metrics) != 1 || len(snap.Apps[0].Volumes) != 1 {
		t.Fatal("snapshot lost modeled configuration")
	}
	want := "# keep\nPUBLIC=https://source.test\nSECRET=\nDB="
	if snap.Apps[0].Config.EnvText != want {
		t.Fatal("incorrect sanitized text")
	}
	if err := q.UpdateApplicationEnv(ctx, db.UpdateApplicationEnvParams{ID: app.ID, EnvText: "PUBLIC=newer"}); err != nil {
		t.Fatal(err)
	}
	if snap.Apps[0].Config.EnvText != want {
		t.Fatal("source edit changed captured target values")
	}
	t.Run("publication uses captured values and refuses removed dependencies", func(t *testing.T) {
		svc := New(pool, nil, nil, "clone.test")
		defer svc.Shutdown(context.Background())
		req := Request{UserID: u.ID, OrgID: o.ID, ProjectID: p.ID, SourceID: e.ID, Name: "snapshot-copy"}
		intent := ResourceIntent{Databases: []DatabaseIntent{{Database: dbservice.CloneDatabase{DBName: "new_db", Username: "new_user"}}}}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		target, err := svc.publish(ctx, tx, req, snap, intent, map[int64]string{ld.ID: "new-password"})
		if err != nil {
			t.Fatal(err)
		}
		apps, err := db.New(tx).ListApplicationsByEnvironment(ctx, target)
		if err != nil || len(apps) != 1 || apps[0].EnvText != want {
			t.Fatal("publication reread live source values")
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		// Remove a captured logical dependency in the same transaction. A final
		// identity check must refuse, even though the snapshot is still usable.
		tx, err = pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := db.New(tx).DeleteLogicalDatabase(ctx, ld.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.publish(ctx, tx, req, snap, intent, map[int64]string{ld.ID: "new-password"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted dependency resurrected: %v", err)
		}
	})

	if _, err := ReadSnapshot(ctx, pool, o.ID+1, p.ID, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("org mismatch was accepted")
	}
	if _, err := ReadSnapshot(ctx, pool, o.ID, p.ID, e.ID+1); !errors.Is(err, ErrNotFound) {
		t.Fatal("environment mismatch was accepted")
	}
	redis, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Name: "redis", Engine: "redis", AppName: "fixture-redis", Image: "redis:7", SuperuserPassword: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, InstanceID: &redis.ID, VarName: "CACHE", Scheme: "redis", Field: "url"}); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSnapshot(ctx, pool, o.ID, p.ID, e.ID); err != nil || len(got.Instances) != 1 || got.Instances[0].SourceID != redis.ID {
		t.Fatal("non-PG source was not captured")
	}
}
