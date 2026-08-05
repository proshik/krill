package volume

import (
	"context"
	"errors"
	"testing"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

// Mirrors backup.TestGetDestinationFailsOnUndecryptableKeys: a volume backup
// must not be handed empty S3 credentials when the stored keys cannot be
// decrypted — it would fail at the far end of the pipeline, long after the
// archive sidecar has already run.
func TestGetDestinationFailsOnUndecryptableKeys(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "vdest@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O", Slug: "o-vdest", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("org: %v", err)
	}

	secret.Init("old-key")
	ak, sk := secret.Enc("AKIAEXAMPLE"), secret.Enc("s3cret")
	secret.Init("new-key")
	defer secret.Init("")

	dest, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "minio", Endpoint: "http://localhost:9000",
		Bucket: "b", Region: "us-east-1", AccessKey: ak, SecretKey: sk,
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}

	got, err := NewDBStore(q).GetDestination(ctx, dest.ID)
	if err == nil {
		t.Fatalf("expected an error for undecryptable keys, got %+v", got)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
	}
}

func TestDBVolumeStoreListEnabledBackups(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	// Minimal FK chain: user → org → project → env → application → app_volume.
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "u@k", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O", Slug: "o", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	p, err := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	e, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "prod", Slug: "prod"})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "w.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	v, err := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: a.ID, Name: "data", MountPath: "/data"})
	if err != nil {
		t.Fatalf("volume: %v", err)
	}
	dest, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "minio", Endpoint: "http://localhost:9000",
		Bucket: "b", Region: "us-east-1", AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}

	enabled, err := q.CreateVolumeBackup(ctx, db.CreateVolumeBackupParams{
		AppVolumeID: v.ID, DestinationID: dest.ID, Schedule: "0 3 * * *", Prefix: "vol", Retention: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("enabled backup: %v", err)
	}
	if _, err := q.CreateVolumeBackup(ctx, db.CreateVolumeBackupParams{
		AppVolumeID: v.ID, DestinationID: dest.ID, Schedule: "0 4 * * *", Prefix: "vol2", Retention: 3, Enabled: false,
	}); err != nil {
		t.Fatalf("disabled backup: %v", err)
	}

	store := NewDBStore(q)
	got, err := store.ListEnabledBackups(ctx)
	if err != nil {
		t.Fatalf("ListEnabledBackups: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 enabled backup, got %d: %+v", len(got), got)
	}
	want := backup.SchedBackup{ID: enabled.ID, Schedule: "0 3 * * *"}
	if got[0] != want {
		t.Fatalf("ListEnabledBackups = %+v, want %+v", got[0], want)
	}
}
