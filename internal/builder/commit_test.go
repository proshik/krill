package builder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// headCommit reads the checked-out commit of a real repository, shortened to
// seven hex digits; a directory that is not a repository yields "".
func TestHeadCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "Dockerfile")
	git("commit", "-q", "-m", "init")
	full := git("rev-parse", "HEAD")

	if got := headCommit(context.Background(), dir); got != full[:7] {
		t.Fatalf("headCommit = %q, want %q", got, full[:7])
	}
	if got := headCommit(context.Background(), t.TempDir()); got != "" {
		t.Fatalf("not a repository: %q", got)
	}
}

func TestShortCommit(t *testing.T) {
	for in, want := range map[string]string{
		"1a09ec0f3b2c": "1a09ec0",
		"1a09ec0":      "1a09ec0",
		"1a09ec":       "",
		"fatal: x":     "",
		"1A09EC0F":     "",
	} {
		if got := shortCommit(in); got != want {
			t.Errorf("shortCommit(%q) = %q, want %q", in, got, want)
		}
	}
}
