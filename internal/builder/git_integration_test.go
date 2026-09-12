//go:build integration

package builder_test

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/builder"
)

// A small public repository with a Dockerfile at the root.
const testRepo = "https://github.com/dockersamples/helloworld-demo-node.git"

func TestGitBuilderBuildsImage(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	tag := "krill-it:builder-test"
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })

	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	err := builder.New("", false).Build(ctx, builder.BuildRequest{
		AppID: 1, DeployID: 1,
		GitURL: testRepo, GitBranch: "main", DockerfilePath: "Dockerfile",
		ImageTag: tag,
	}, &buf)
	if err != nil {
		t.Fatalf("build: %v\n--- log ---\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "build complete") {
		t.Errorf("log missing completion marker:\n%s", buf.String())
	}
	// image exists
	if out, err := exec.Command("docker", "image", "inspect", tag).CombinedOutput(); err != nil {
		t.Fatalf("image not found: %v\n%s", err, out)
	}
}
