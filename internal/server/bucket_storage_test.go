package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

func TestSameStorage(t *testing.T) {
	d := func(endpoint, bucket string) db.Destination {
		return db.Destination{Endpoint: endpoint, Bucket: bucket}
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"https://s3.example.com", "https://s3.example.com:443", true},
		{"http://s3.example.com", "http://s3.example.com:80/", true},
		{"HTTPS://S3.Example.com/", "https://s3.example.com", true},
		{"s3.example.com", "https://s3.example.com", true},
		{"https://s3.example.com", "https://s3.example.com:9000", false},
		{"http://s3.example.com", "https://s3.example.com", false},
		{"https://s3.example.com", "https://s4.example.com", false},
	} {
		if got := sameStorage(d(c.a, "bk"), d(c.b, "BK")); got != c.same {
			t.Errorf("sameStorage(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
	if sameStorage(d("https://s3.example.com", "a"), d("https://s3.example.com", "b")) {
		t.Error("different buckets are different storage")
	}
}

// execLog is an engine that records the commands run in containers.
type execLog struct {
	docker.Engine
	mu   sync.Mutex
	cmds []string
}

func (e *execLog) Exec(_ context.Context, _ string, cmd []string, _ []string, _ io.Reader, out io.Writer) error {
	e.mu.Lock()
	e.cmds = append(e.cmds, strings.Join(cmd, " "))
	e.mu.Unlock()
	io.WriteString(out, `{"status":"success"}`+"\n")
	return nil
}

func (e *execLog) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cmds)
}

// A refused restore removes the bucket it created for itself only while
// nothing uses it: an app linked to it meanwhile, or another operation holding
// its lock, keeps the bucket, its key and its row.
func TestDropUnusedRestoreTargetKeepsABucketInUse(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "drop@k.local", PasswordHash: "x"})
	svc := org.NewService(q)
	o, _ := svc.CreateOrg(ctx, u.ID, "Org")
	p, _ := svc.CreateProject(ctx, o.ID, "P", "")
	e, _ := svc.CreateEnvironment(ctx, p.ID, "prod")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "backend", Image: "img", Tag: "1", Domain: "d.x", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	inst, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "minio", Name: "silo", AppName: "krill-minio-drop-t1",
		Image: "img", Superuser: "rootadmin", SuperuserPassword: "pw",
	})
	eng := &execLog{}
	s := &Server{q: q, dbsvc: dbservice.New(eng, dbservice.NewDBStore(q), deploy.NewLogHub(), "krill-net")}
	di := dbservice.Instance{AppName: inst.AppName, Superuser: "rootadmin", SuperuserPassword: "pw"}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	mk := func(name string) db.S3Bucket {
		b, err := q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: inst.ID, EnvironmentID: e.ID, Name: name, AccessKey: "krill-" + name + "-aaaaaa", SecretKey: "s", CreatedByKrill: true, AccessKeyGenerated: true})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	exists := func(id int64) bool { _, err := q.GetS3Bucket(ctx, id); return err == nil }

	linked := mk("linked")
	if _, err := q.CreateBucketLink(ctx, db.CreateBucketLinkParams{ApplicationID: app.ID, BucketID: linked.ID, VarName: "S3_BUCKET", Field: "bucket"}); err != nil {
		t.Fatal(err)
	}
	s.dropUnusedRestoreTarget(req, di, linked)
	if eng.count() != 0 || !exists(linked.ID) {
		t.Fatalf("a linked target was touched: %v, row kept %v", eng.cmds, exists(linked.ID))
	}

	busy := mk("busy")
	oplock.TryAcquire(oplock.Bucket(busy.ID))
	s.dropUnusedRestoreTarget(req, di, busy)
	oplock.Release(oplock.Bucket(busy.ID))
	if eng.count() != 0 || !exists(busy.ID) {
		t.Fatalf("a locked target was touched: %v", eng.cmds)
	}

	unused := mk("unused")
	s.dropUnusedRestoreTarget(req, di, unused)
	if exists(unused.ID) || eng.count() != 2 || !strings.Contains(eng.cmds[0], "rb k/unused") || !strings.Contains(eng.cmds[1], "admin user remove k krill-unused-aaaaaa") {
		t.Fatalf("an unused target must go: row %v, commands %v", exists(unused.ID), eng.cmds)
	}
}
