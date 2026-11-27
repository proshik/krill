package backup_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/netguard"
	"github.com/proshik/krill/internal/testutil"
)

func TestS3Roundtrip(t *testing.T) {
	ctx := context.Background()
	m := testutil.NewMinio(t)
	d := backup.Destination{Endpoint: m.Endpoint, Bucket: m.Bucket(t, "test"), Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	// The MinIO test container binds to a loopback endpoint (http://localhost:<port>
	// or http://127.0.0.1:<port>), so the roundtrip itself must run with
	// allowPrivate=true — exactly the loopback traffic the SSRF guard blocks by
	// default. See TestS3CheckAccessBlocksLoopbackByDefault below for the guard
	// itself wired in with allowPrivate=false.
	if err := backup.CreateBucket(ctx, d, true); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := backup.CheckAccess(ctx, d, true); err != nil {
		t.Fatalf("check access: %v", err)
	}
	if err := backup.Upload(ctx, d, "p/db/a.sql.gz", strings.NewReader("hello"), true); err != nil {
		t.Fatalf("upload: %v", err)
	}
	objs, err := backup.List(ctx, d, "p/db/", true)
	if err != nil || len(objs) != 1 || objs[0].Key != "p/db/a.sql.gz" {
		t.Fatalf("list = %+v err=%v", objs, err)
	}
	rc, err := backup.Download(ctx, d, "p/db/a.sql.gz", true)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("download = %q", b)
	}
	if err := backup.Delete(ctx, d, "p/db/a.sql.gz", true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if objs, _ := backup.List(ctx, d, "p/db/", true); len(objs) != 0 {
		t.Fatalf("expected empty, got %d", len(objs))
	}
}

// TestS3CheckAccessBlocksLoopbackByDefault verifies the SSRF egress guard is
// actually wired into the S3 client: CheckAccess against a loopback endpoint
// (as a maliciously-configured "S3 destination" pointing at an internal
// service would be) must fail closed when allowPrivate=false.
func TestS3CheckAccessBlocksLoopbackByDefault(t *testing.T) {
	ctx := context.Background()
	m := testutil.NewMinio(t)
	d := backup.Destination{Endpoint: m.Endpoint, Bucket: m.Bucket(t, "test"), Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	err := backup.CheckAccess(ctx, d, false)
	if err == nil {
		t.Fatal("CheckAccess(allowPrivate=false) against a loopback endpoint: expected error, got nil")
	}
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("CheckAccess(allowPrivate=false) against a loopback endpoint: expected netguard.ErrBlocked, got %v", err)
	}
}

// TestHead reports the stored object's size, which the state backup compares
// with the bytes it streamed.
func TestHead(t *testing.T) {
	ctx := context.Background()
	m := testutil.NewMinio(t)
	d := backup.Destination{Endpoint: m.Endpoint, Bucket: m.Bucket(t, "test"), Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	if err := backup.CreateBucket(ctx, d, true); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := backup.Upload(ctx, d, "k/ten", strings.NewReader("0123456789"), true); err != nil {
		t.Fatalf("upload: %v", err)
	}
	n, err := backup.Head(ctx, d, "k/ten", true)
	if err != nil || n != 10 {
		t.Fatalf("Head = %d, %v; want 10", n, err)
	}
	if _, err := backup.Head(ctx, d, "k/missing", true); err == nil {
		t.Fatal("Head of a missing object: want an error")
	}
}

// A listing entry without a key (an S3-compatible server can send one) is a
// listing error, not a nil dereference in the caller's goroutine.
func TestListRejectsEntryWithoutKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<Name>b</Name><Prefix></Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
<Contents><Key>ok.sql.gz</Key><Size>3</Size></Contents>
<Contents><Size>3</Size></Contents>
</ListBucketResult>`)
	}))
	defer srv.Close()
	d := backup.Destination{Endpoint: srv.URL, Bucket: "b", Region: "us-east-1", AccessKey: "ak", SecretKey: "sk"}
	objs, err := backup.List(context.Background(), d, "", true)
	if err == nil || !strings.Contains(err.Error(), "without a key") {
		t.Fatalf("List = %+v, %v; want an error about a missing key", objs, err)
	}
}
