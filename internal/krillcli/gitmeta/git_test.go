package gitmeta_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/proshik/krill/internal/krillcli/gitmeta"
)

// TestDescribeScopesDirtinessToItsDirectory is the monorepo case the layout
// this CLI advertises produces directly: one krill.yaml per service, and
// project.Find walking up to the nearest one. An unscoped `git status` reports
// the WHOLE repository, so editing services/web marked a deploy of
// services/bot dirty — tagging the image -dirty for changes that are not in
// its build context and cannot reach its image, and refusing the build
// outright under require_clean.
func TestDescribeScopesDirtinessToItsDirectory(t *testing.T) {
	if !gitmeta.Available() {
		t.Skip("git is not on PATH")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	bot := filepath.Join(repo, "services", "bot")
	web := filepath.Join(repo, "services", "web")
	for _, d := range []string{bot, web} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	run("add", ".")
	run("commit", "-qm", "initial")

	ctx := context.Background()
	if info, err := gitmeta.Describe(ctx, bot); err != nil {
		t.Fatalf("describe: %v", err)
	} else if info.Dirty {
		t.Fatalf("a freshly committed tree reports dirty: %+v", info)
	}

	// Change the OTHER service.
	if err := os.WriteFile(filepath.Join(web, "main.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := gitmeta.Describe(ctx, bot)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if info.Dirty {
		t.Fatalf("services/bot reports dirty because services/web changed: %+v", info)
	}
	if wi, err := gitmeta.Describe(ctx, web); err != nil {
		t.Fatalf("describe: %v", err)
	} else if !wi.Dirty {
		t.Fatal("services/web should report its own change as dirty")
	} else if len(wi.DirtyPaths) != 1 || wi.DirtyPaths[0] != "services/web/main.go" {
		// Exact, not merely non-empty: the status field is two columns and a
		// space, but the whole output is trimmed before it is split, so a
		// fixed offset silently eats the first character of the first path.
		t.Fatalf("dirty paths = %q, want exactly [services/web/main.go]", wi.DirtyPaths)
	}
}
