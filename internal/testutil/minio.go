package testutil

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/proshik/krill/internal/dbservice/drivers"
)

// MinioInfo holds connection details for a disposable MinIO container.
type MinioInfo struct {
	Endpoint  string // http://host:mappedport
	AccessKey string
	SecretKey string
	Region    string // "us-east-1"
}

// NewMinio spins up a disposable MinIO container and returns its connection
// info. The container is terminated when the test finishes.
func NewMinio(t *testing.T) MinioInfo {
	t.Helper()
	ctx := context.Background()

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
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}

	endpoint, err := c.PortEndpoint(ctx, "9000/tcp", "http")
	if err != nil {
		t.Fatalf("minio endpoint: %v", err)
	}

	return MinioInfo{
		Endpoint:  endpoint,
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		Region:    "us-east-1",
	}
}
