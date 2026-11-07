//go:build integration

package dbservice_test

import (
	"bytes"
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

	// Import never touches existing objects, including one named like the old
	// fixed probe.
	for _, k := range []string{".krill-import-check", "keep.txt"} {
		if err := svc.WriteObject(ctx, inst, "penpot", k, bytes.NewReader([]byte("user data "+k))); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	if err := svc.ImportBucket(ctx, inst, assets); err != nil {
		t.Fatalf("import over existing objects: %v", err)
	}
	for _, k := range []string{".krill-import-check", "keep.txt"} {
		var got bytes.Buffer
		if err := svc.ReadObject(ctx, inst, "penpot", k, &got); err != nil || got.String() != "user data "+k {
			t.Fatalf("import changed %s: %q %v", k, got.String(), err)
		}
	}
	if objs, err := svc.ListObjects(ctx, inst, "penpot", 10); err != nil || len(objs) != 2 {
		t.Fatalf("import must leave no probe behind: %+v %v", objs, err)
	}
	if _, err := svc.ListObjects(ctx, inst, "penpot", 1); !errors.Is(err, dbservice.ErrTooManyObjects) {
		t.Fatalf("listing past its limit: %v", err)
	}
	// A principal that can write and delete but not read is refused: links
	// hand apps a key that must read their bucket back, and the probe it
	// wrote is removed all the same. (mc's rm stats the object first, which
	// needs s3:GetObject too, so through mc the refusal shows at the read-back
	// and at the principal's own rm alike; the read-back stays as the explicit
	// check, not relying on that detail of mc.)
	root := []string{"MC_HOST_k=http://rootadmin:root-secret-123@127.0.0.1:9000"}
	writeOnly := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::penpot/*"]}]}`
	for _, step := range []struct {
		cmd   []string
		stdin string
	}{
		{[]string{"mc", "--json", "admin", "policy", "create", "k", "it-write-only", "/dev/stdin"}, writeOnly},
		{[]string{"mc", "--json", "admin", "user", "add", "k", "it-writer", "writer-secret-1"}, ""},
		{[]string{"mc", "--json", "admin", "policy", "attach", "k", "it-write-only", "--user", "it-writer"}, ""},
	} {
		var out bytes.Buffer
		if err := eng.Exec(ctx, inst.AppName, step.cmd, root, bytes.NewReader([]byte(step.stdin)), &out); err != nil {
			t.Fatalf("%v: %v %s", step.cmd[2:5], err, out.String())
		}
	}
	if err := svc.ImportBucket(ctx, inst, dbservice.Bucket{Name: "penpot", AccessKey: "it-writer", SecretKey: "writer-secret-1"}); !errors.Is(err, dbservice.ErrBucketCredentials) {
		t.Fatalf("a principal without read access must be refused, got %v", err)
	}
	if objs, err := svc.ListObjects(ctx, inst, "penpot", 10); err != nil || len(objs) != 2 {
		t.Fatalf("a refused import must leave no probe behind: %+v %v", objs, err)
	}

	// Undoing a bucket only ever removes an empty one.
	if err := svc.RemoveEmptyBucket(ctx, inst, "penpot"); err == nil {
		t.Fatal("a non-empty bucket must not be removed by RemoveEmptyBucket")
	}
	if err := svc.ReadObject(ctx, inst, "penpot", "keep.txt", io.Discard); err != nil {
		t.Fatalf("data gone after a refused removal: %v", err)
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
