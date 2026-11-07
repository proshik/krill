//go:build integration

package bucketbackup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/bucketbackup"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/testutil"
)

type store struct {
	row     bucketbackup.Row
	targets map[int64]bucketbackup.Target
	dst     backup.Destination
	restore chan string
}

func (s *store) GetBackup(context.Context, int64) (bucketbackup.Row, error) { return s.row, nil }
func (s *store) GetTarget(_ context.Context, id int64) (bucketbackup.Target, error) {
	return s.targets[id], nil
}
func (s *store) GetDestination(context.Context, int64) (backup.Destination, error) {
	return s.dst, nil
}
func (s *store) SetBackupResult(context.Context, int64, time.Time, string, string) error { return nil }
func (s *store) SetRestoreResult(_ context.Context, _ int64, _ time.Time, status, msg string) error {
	s.restore <- status + " " + msg
	return nil
}

// A real Silo instance on the local swarm: objects with nested keys and
// spaces are snapshotted through mc into a separate S3 storage, and restored
// byte for byte into a new bucket.
func TestBucketBackupAndRestoreOnSilo(t *testing.T) {
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := eng.NetworkEnsure(ctx, "krill-net"); err != nil {
		t.Skipf("swarm unavailable: %v", err)
	}
	d, _ := drivers.Registry.Lookup("minio")
	inst := drivers.Instance{ID: 990003, Engine: "minio", AppName: "krill-it-s3bk", Image: d.DefaultImage(), Superuser: "rootadmin", SuperuserPassword: "root-secret-9"}
	clean := func() {
		_ = eng.ServiceRemove(context.Background(), inst.AppName)
		for i := 0; i < 15 && eng.VolumeRemove(context.Background(), drivers.VolumeName(inst.AppName)) != nil; i++ {
			time.Sleep(time.Second)
		}
	}
	clean()
	t.Cleanup(clean)
	if err := eng.ImagePull(ctx, inst.Image, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := eng.ServiceDeploy(ctx, d.BuildSpec(inst, "krill-net")); err != nil {
		t.Fatal(err)
	}
	svc := dbservice.New(eng, nil, nil, "krill-net")
	deadline := time.Now().Add(90 * time.Second)
	for {
		if err = svc.CreateBucket(ctx, inst, dbservice.Bucket{Name: "assets", AccessKey: "krill-assets-aaaaaa", SecretKey: "assets-secret-1"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create bucket: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
	objects := map[string][]byte{
		"a.png":               bytes.Repeat([]byte{0, 1, 2, 255}, 300000), // 1.2 MB, binary
		"dir one/sub/f 1.txt": []byte("hello world"),
	}
	for k, v := range objects {
		if err := svc.WriteObject(ctx, inst, "assets", k, bytes.NewReader(v)); err != nil {
			t.Fatalf("seed %q: %v", k, err)
		}
	}

	m := testutil.NewMinio(t)
	dst := backup.Destination{Endpoint: m.Endpoint, Bucket: "backups", Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	if err := backup.CreateBucket(ctx, dst, true); err != nil {
		t.Fatal(err)
	}
	src := bucketbackup.Target{Server: inst.AppName, Bucket: "assets", RootUser: inst.Superuser, RootPass: inst.SuperuserPassword}
	st := &store{row: bucketbackup.Row{ID: 1, BucketID: 1, Retention: 3}, targets: map[int64]bucketbackup.Target{1: src}, dst: dst, restore: make(chan string, 1)}
	bb := bucketbackup.New(bucketbackup.MCIO{Svc: svc}, st, true)

	if err := bb.RunBackup(ctx, 1, time.Now()); err != nil {
		t.Fatalf("backup: %v", err)
	}
	snaps, err := bb.Snapshots(ctx, 1)
	if err != nil || len(snaps) != 1 || !snaps[0].Complete || snaps[0].Objects != 2 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	if err := svc.CreateBucket(ctx, inst, dbservice.Bucket{Name: "assets-restored", AccessKey: "krill-assets-bbbbbb", SecretKey: "assets-secret-2"}); err != nil {
		t.Fatal(err)
	}
	target := bucketbackup.Target{Server: inst.AppName, Bucket: "assets-restored", RootUser: inst.Superuser, RootPass: inst.SuperuserPassword}
	if err := bb.StartRestore(ctx, 1, snaps[0].Timestamp, target, 5*time.Minute); err != nil {
		t.Fatalf("restore: %v", err)
	}
	select {
	case got := <-st.restore:
		if got != "ok " {
			t.Fatalf("restore result: %q", got)
		}
	case <-time.After(3 * time.Minute):
		t.Fatal("restore did not finish")
	}
	for k, want := range objects {
		var buf bytes.Buffer
		if err := svc.ReadObject(ctx, inst, "assets-restored", k, &buf); err != nil {
			t.Fatalf("read restored %q: %v", k, err)
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("restored %q differs: %d bytes, want %d", k, buf.Len(), len(want))
		}
	}
	// The restored bucket is no longer empty: a second restore into it is refused.
	if err := bb.StartRestore(ctx, 1, snaps[0].Timestamp, target, time.Minute); !errors.Is(err, bucketbackup.ErrTargetNotEmpty) {
		t.Fatalf("restore into a non-empty bucket: %v", err)
	}
}
