package testutil

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/proshik/krill/internal/dbservice/drivers"
)

// MinioInfo holds connection details for the shared S3 server of a test
// binary.
type MinioInfo struct {
	Endpoint  string // http://host:mappedport
	AccessKey string
	SecretKey string
	Region    string // "us-east-1"
}

var (
	minioOnce      sync.Once
	sharedMinio    MinioInfo
	sharedMinioErr error
	bucketCounter  atomic.Int64
)

// NewMinio returns the connection info of an S3 server — the image a managed
// S3 instance runs (Silo, a MinIO fork) — shared by every test of the binary.
// It is started on the first call and lives as long as the process:
// testcontainers' reaper (Ryuk) removes the container after the binary exits.
//
// Buckets are server-wide, so a test keeps to its own: it names every bucket
// it creates with Bucket, and must not list all buckets, change the server's
// configuration, or create users or policies on it. A test that needs that
// starts its own container.
func NewMinio(t *testing.T) MinioInfo {
	t.Helper()
	minioOnce.Do(func() { sharedMinio, sharedMinioErr = startSharedMinio(context.Background()) })
	if sharedMinioErr != nil {
		t.Fatalf("shared minio: %v", sharedMinioErr)
	}
	return sharedMinio
}

// Bucket returns a bucket name no other call in the test binary returns:
// base, a counter and the test name, made a valid S3 bucket name (3–63
// lowercase letters, digits and hyphens, starting and ending with a letter or
// digit). It does not create the bucket. Buckets are left on the server when
// the test ends; the server goes away with the process, and no other test
// can reach a bucket it was not given the name of.
func (MinioInfo) Bucket(t *testing.T, base string) string {
	t.Helper()
	return bucketName(base, t.Name(), bucketCounter.Add(1))
}

func startSharedMinio(ctx context.Context) (MinioInfo, error) {
	c, err := testcontainers.Run(ctx,
		drivers.MinIOImage, // the same image a managed MinIO instance runs
		testcontainers.WithCmd("server", "/data"),
		testcontainers.WithEnv(map[string]string{
			"MINIO_ROOT_USER":     "minioadmin",
			"MINIO_ROOT_PASSWORD": "minioadmin",
		}),
		testcontainers.WithExposedPorts("9000/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/minio/health/live").
				WithPort("9000/tcp").
				WithStatusCodeMatcher(func(status int) bool { return status == http.StatusOK }).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if c != nil {
			_ = c.Terminate(ctx)
		}
		return MinioInfo{}, fmt.Errorf("start minio: %w", err)
	}
	endpoint, err := c.PortEndpoint(ctx, "9000/tcp", "http")
	if err != nil {
		return MinioInfo{}, fmt.Errorf("minio endpoint: %w", err)
	}
	return MinioInfo{
		Endpoint:  endpoint,
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		Region:    "us-east-1",
	}, nil
}

// bucketName builds "<base>-<n>-<test name>" in lowercase letters, digits and
// single hyphens, cut to S3's 63-character limit. The base is cut to 20
// characters first so the counter, which makes the name unique, always fits.
func bucketName(base, testName string, n int64) string {
	b := sanitizeBucketPart(base)
	if len(b) > 20 {
		b = strings.TrimRight(b[:20], "-")
	}
	if b == "" {
		b = "bucket"
	}
	name := fmt.Sprintf("%s-%d", b, n)
	if rest := sanitizeBucketPart(testName); rest != "" {
		name += "-" + rest
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// sanitizeBucketPart lowercases s, turns every run of other characters into
// one hyphen and trims hyphens from both ends.
func sanitizeBucketPart(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
