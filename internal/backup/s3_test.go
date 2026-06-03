package backup_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/testutil"
)

func TestS3Roundtrip(t *testing.T) {
	ctx := context.Background()
	m := testutil.NewMinio(t)
	d := backup.Destination{Endpoint: m.Endpoint, Bucket: "test", Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	if err := backup.CreateBucket(ctx, d); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := backup.CheckAccess(ctx, d); err != nil {
		t.Fatalf("check access: %v", err)
	}
	if err := backup.Upload(ctx, d, "p/db/a.sql.gz", strings.NewReader("hello")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	objs, err := backup.List(ctx, d, "p/db/")
	if err != nil || len(objs) != 1 || objs[0].Key != "p/db/a.sql.gz" {
		t.Fatalf("list = %+v err=%v", objs, err)
	}
	rc, err := backup.Download(ctx, d, "p/db/a.sql.gz")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("download = %q", b)
	}
	if err := backup.Delete(ctx, d, "p/db/a.sql.gz"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if objs, _ := backup.List(ctx, d, "p/db/"); len(objs) != 0 {
		t.Fatalf("expected empty, got %d", len(objs))
	}
}
