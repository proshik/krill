package project_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/krillcli/project"
)

const minimal = `
app: acme/production/bot
image:
  repository: ghcr.io/acme/bot
`

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, project.FileName)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadAppliesDefaults(t *testing.T) {
	p := writeConfig(t, t.TempDir(), minimal)
	c, err := project.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Image.Platform != project.DefaultPlatform {
		t.Fatalf("platform = %q, want the server default %q", c.Image.Platform, project.DefaultPlatform)
	}
	if c.Build.Dockerfile != "Dockerfile" || c.Build.Context != "." {
		t.Fatalf("build defaults: %+v", c.Build)
	}
	if c.Delivery != project.DeliveryRegistry {
		t.Fatalf("delivery = %q, want registry", c.Delivery)
	}
	if c.Tag.Strategy != "git" {
		t.Fatalf("tag strategy = %q, want git", c.Tag.Strategy)
	}
}

// TestDefaultPlatformIsNotTheHost pins the deliberate choice. Defaulting to
// runtime.GOARCH would silently produce an arm64 image on a Mac for an amd64
// server, and that failure only shows up as a dead container.
func TestDefaultPlatformIsNotTheHost(t *testing.T) {
	if project.DefaultPlatform != "linux/amd64" {
		t.Fatalf("DefaultPlatform = %q; changing it re-opens the arm64-Mac footgun", project.DefaultPlatform)
	}
}

// TestUnknownKeyIsAnError is the guard that keeps secrets out of a committed
// file and catches typos in it.
func TestUnknownKeyIsAnError(t *testing.T) {
	for _, body := range []string{
		minimal + "token: krill_pat_secret\n",
		minimal + "platfrom: linux/arm64\n",
		"app: a/b/c\nimage:\n  repository: r\n  platfrom: linux/amd64\n",
	} {
		p := writeConfig(t, t.TempDir(), body)
		if _, err := project.Load(p); err == nil {
			t.Fatalf("unknown key must fail to parse:\n%s", body)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name, body, wantSubstr string
	}{
		{"no app", "image:\n  repository: r\n", `"app" is required`},
		{"two segment app", "app: production/bot\nimage:\n  repository: r\n", "three parts"},
		{"empty segment", "app: acme//bot\nimage:\n  repository: r\n", "empty path segment"},
		{"no repository", "app: a/b/c\n", `"image.repository" is required`},
		{"bad repository", "app: a/b/c\nimage:\n  repository: \"!!!\"\n", "image.repository"},
		{"bad delivery", minimal + "delivery: carrier-pigeon\n", `"delivery" must be`},
		{"bad strategy", minimal + "tag:\n  strategy: vibes\n", `"tag.strategy"`},
		{"bad prefix", minimal + "tag:\n  prefix: \"-\"\n", `"tag.prefix"`},
		{"bad build arg", minimal + "build:\n  args:\n    \"1BAD\": x\n", `"build.args"`},
		{"absolute dockerfile", minimal + "build:\n  dockerfile: /etc/Dockerfile\n", "must be relative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := writeConfig(t, t.TempDir(), tc.body)
			_, err := project.Load(p)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q should mention %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestNumericAppRefIsAccepted(t *testing.T) {
	p := writeConfig(t, t.TempDir(), "app: \"17\"\nimage:\n  repository: ghcr.io/acme/bot\n")
	if _, err := project.Load(p); err != nil {
		t.Fatalf("a numeric app id must be accepted: %v", err)
	}
}

// TestFindTakesTheNearestFile covers the monorepo case: one file per service,
// and running inside a service must pick that service, not the repo root's.
func TestFindTakesTheNearestFile(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, minimal)

	svc := filepath.Join(root, "services", "bot")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := writeConfig(t, svc, "app: acme/production/other\nimage:\n  repository: ghcr.io/acme/other\n")

	deeper := filepath.Join(svc, "cmd", "x")
	if err := os.MkdirAll(deeper, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := project.Find(deeper)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != inner {
		t.Fatalf("Find picked %q, want the nearest %q", got, inner)
	}
}

func TestFindReportsNotFound(t *testing.T) {
	_, err := project.Find(t.TempDir())
	if !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestBuildPathsResolveAgainstTheFile(t *testing.T) {
	root := t.TempDir()
	svc := filepath.Join(root, "svc")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := writeConfig(t, svc, minimal+"build:\n  context: app\n  dockerfile: docker/Dockerfile\n")
	c, err := project.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := filepath.Join(svc, "app"); c.BuildContextPath() != want {
		t.Fatalf("context = %q, want %q", c.BuildContextPath(), want)
	}
	if want := filepath.Join(svc, "app", "docker", "Dockerfile"); c.DockerfilePath() != want {
		t.Fatalf("dockerfile = %q, want %q", c.DockerfilePath(), want)
	}
}
