//go:build integration

package dbservice_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
)

// TestBucketLifecycleOnSilo runs a managed S3 instance from the driver's own
// spec on the local swarm and drives bucket management through Engine.Exec
// and the image's mc: a created bucket's principal reads and writes it and
// nothing else; an existing bucket and principal are adopted without being
// recreated; a rotation's new principal works and the revoked one does not;
// removing the bucket removes its objects.
func TestBucketLifecycleOnSilo(t *testing.T) {
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	skipNoSwarm(t, eng)
	ctx := context.Background()
	d, err := drivers.Registry.Lookup("minio")
	if err != nil {
		t.Fatal(err)
	}
	inst := drivers.Instance{
		ID: 990001, Engine: "minio", AppName: "krill-it-s3", Image: d.DefaultImage(),
		Superuser: "rootadmin", SuperuserPassword: "root-secret-123",
	}
	spec := d.BuildSpec(inst, "krill-net")
	// Start from an empty volume, and leave none behind: a volume is "in use"
	// for a few seconds after its service is removed.
	clean := func() {
		_ = eng.ServiceRemove(context.Background(), inst.AppName)
		for i := 0; i < 15; i++ {
			if eng.VolumeRemove(context.Background(), drivers.VolumeName(inst.AppName)) == nil {
				return
			}
			time.Sleep(time.Second)
		}
		t.Logf("volume %s not removed", drivers.VolumeName(inst.AppName))
	}
	clean()
	t.Cleanup(clean)
	if err := eng.ImagePull(ctx, spec.Image, io.Discard); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if err := eng.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	svc := dbservice.New(eng, nil, nil, "krill-net")
	waitS3(t, ctx, svc, inst)

	assets := dbservice.Bucket{Name: "penpot", AccessKey: "krill-penpot-aaaa", SecretKey: "assets-secret-1"}
	if err := svc.CreateBucket(ctx, inst, assets); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	// The principal can use its bucket — ImportBucket proves exactly that.
	if err := svc.ImportBucket(ctx, inst, assets); err != nil {
		t.Fatalf("principal cannot use its own bucket: %v", err)
	}
	// ...and nothing else.
	other := dbservice.Bucket{Name: "other-bucket", AccessKey: "krill-other-aaaa", SecretKey: "other-secret-1"}
	if err := svc.CreateBucket(ctx, inst, other); err != nil {
		t.Fatalf("create second bucket: %v", err)
	}
	cross := dbservice.Bucket{Name: "other-bucket", AccessKey: assets.AccessKey, SecretKey: assets.SecretKey}
	if err := svc.ImportBucket(ctx, inst, cross); !errors.Is(err, dbservice.ErrBucketCredentials) {
		t.Fatalf("a bucket's principal must not reach another bucket, got %v", err)
	}
	if err := svc.ImportBucket(ctx, inst, dbservice.Bucket{Name: "no-such-bucket", AccessKey: "x12", SecretKey: "y12345678"}); !errors.Is(err, dbservice.ErrBucketMissing) {
		t.Fatalf("importing a missing bucket: want ErrBucketMissing, got %v", err)
	}
	// A creation that collides with an existing bucket fails and leaves it.
	if err := svc.CreateBucket(ctx, inst, dbservice.Bucket{Name: "penpot", AccessKey: "krill-penpot-cccc", SecretKey: "dup-secret-1"}); err == nil {
		t.Fatal("creating an existing bucket must fail")
	}
	if err := svc.ImportBucket(ctx, inst, assets); err != nil {
		t.Fatalf("a failed duplicate create must not touch the existing bucket: %v", err)
	}

	// Rotation: a second principal for the same bucket, then the first revoked.
	if err := svc.AddPrincipal(ctx, inst, "penpot", "krill-penpot-bbbb", "assets-secret-2"); err != nil {
		t.Fatalf("add principal: %v", err)
	}
	if err := svc.ImportBucket(ctx, inst, dbservice.Bucket{Name: "penpot", AccessKey: "krill-penpot-bbbb", SecretKey: "assets-secret-2"}); err != nil {
		t.Fatalf("rotated principal cannot use the bucket: %v", err)
	}
	if err := svc.RemovePrincipal(ctx, inst, assets.AccessKey); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := svc.ImportBucket(ctx, inst, assets); !errors.Is(err, dbservice.ErrBucketCredentials) {
		t.Fatalf("a revoked principal must stop working, got %v", err)
	}
	if err := svc.RemovePrincipal(ctx, inst, assets.AccessKey); err != nil {
		t.Fatalf("revoking twice must be a no-op: %v", err)
	}

	if err := svc.RemoveBucket(ctx, inst, "penpot"); err != nil {
		t.Fatalf("remove bucket: %v", err)
	}
	if err := svc.ImportBucket(ctx, inst, dbservice.Bucket{Name: "penpot", AccessKey: "krill-penpot-bbbb", SecretKey: "assets-secret-2"}); !errors.Is(err, dbservice.ErrBucketMissing) {
		t.Fatalf("removed bucket still there: %v", err)
	}
}

// waitS3 waits until mc inside the instance answers.
func waitS3(t *testing.T, ctx context.Context, svc *dbservice.Service, inst drivers.Instance) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		err := svc.ImportBucket(ctx, inst, dbservice.Bucket{Name: "krill-readiness", AccessKey: inst.Superuser, SecretKey: inst.SuperuserPassword})
		if errors.Is(err, dbservice.ErrBucketMissing) {
			return // the server answered: the bucket simply does not exist
		}
		if time.Now().After(deadline) {
			t.Fatalf("S3 instance not ready: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}
