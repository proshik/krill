package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

// lockBucket hands the mutation the row as it is under the lock, not as it
// was loaded: a rotation that held the lock a moment earlier changed the keys,
// and a delete removed the row (then the mutation must stop with a 404 and
// leave the lock free).
func TestLockBucketRereadsTheRow(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "lock@k.local", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	svc := org.NewService(q)
	o, _ := svc.CreateOrg(ctx, u.ID, "Org")
	p, _ := svc.CreateProject(ctx, o.ID, "P", "")
	e, _ := svc.CreateEnvironment(ctx, p.ID, "prod")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "minio", Name: "silo", AppName: "krill-minio-lock-t1",
		Image: "img", Superuser: "rootadmin", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: inst.ID, EnvironmentID: e.ID, Name: "penpot", AccessKey: "key-0", SecretKey: "s0", CreatedByKrill: true, AccessKeyGenerated: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := q.RotateS3BucketKey(ctx, db.RotateS3BucketKeyParams{ID: stale.ID, AccessKey: "key-1", SecretKey: "s1"}); err != nil || n != 1 {
		t.Fatalf("rotate: %d %v", n, err)
	}

	s := &Server{q: q}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	c := bucketCtx{Bucket: stale}
	if !s.lockBucket(httptest.NewRecorder(), req, &c) {
		t.Fatal("lock refused")
	}
	oplock.Release(oplock.Bucket(stale.ID))
	if c.Bucket.AccessKey != "key-1" || c.Bucket.PrevAccessKey != "key-0" {
		t.Fatalf("mutation got the stale row: current %q, previous %q", c.Bucket.AccessKey, c.Bucket.PrevAccessKey)
	}

	if err := q.DeleteS3Bucket(ctx, stale.ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c = bucketCtx{Bucket: stale}
	if s.lockBucket(rec, req, &c) || rec.Code != http.StatusNotFound {
		t.Fatalf("a bucket deleted meanwhile: want refused with 404, got %d", rec.Code)
	}
	if !oplock.TryAcquire(oplock.Bucket(stale.ID)) {
		t.Fatal("a refused lockBucket must release the lock")
	}
	oplock.Release(oplock.Bucket(stale.ID))
}
