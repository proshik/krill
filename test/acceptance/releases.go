//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Migration is one extra migration a release variant adds on top of the
// branch's own (the branch ends at 000046).
type Migration struct {
	Name string // file name stem, e.g. "000047_acceptance_a"
	Up   string
	Down string
}

// Variant is one throwaway release built from the current branch.
type Variant struct {
	Tag        string
	Migrations []Migration
	// CrashAfterMigrations makes run() return an error right after the
	// migrations succeed, so the unit crash-loops.
	CrashAfterMigrations bool
}

var (
	migA = Migration{
		Name: "000047_acceptance_a",
		Up:   "CREATE TABLE acceptance_a (id BIGSERIAL PRIMARY KEY, note TEXT);\n",
		Down: "DROP TABLE IF EXISTS acceptance_a;\n",
	}
	migB = Migration{
		Name: "000048_acceptance_b",
		Up:   "ALTER TABLE acceptance_a ADD COLUMN extra TEXT;\n",
		Down: "ALTER TABLE acceptance_a DROP COLUMN IF EXISTS extra;\n",
	}
	migSlow = Migration{
		Name: "000049_acceptance_slow",
		Up:   "SELECT pg_sleep(45); CREATE TABLE acceptance_c (id BIGSERIAL PRIMARY KEY);\n",
		Down: "DROP TABLE IF EXISTS acceptance_c;\n",
	}
	migFail = Migration{
		Name: "000050_acceptance_fail",
		Up:   "ALTER TABLE acceptance_does_not_exist ADD COLUMN x TEXT;\n",
		Down: "ALTER TABLE IF EXISTS acceptance_does_not_exist DROP COLUMN IF EXISTS x;\n",
	}
)

// Variants are the five test releases, oldest first. Migrations are
// cumulative, like real releases.
var Variants = []Variant{
	{Tag: "v0.90.0"},
	{Tag: "v0.90.1", Migrations: []Migration{migA}},
	{Tag: "v0.90.2", Migrations: []Migration{migA, migB}, CrashAfterMigrations: true},
	{Tag: "v0.90.3", Migrations: []Migration{migA, migB, migSlow}},
	{Tag: "v0.90.4", Migrations: []Migration{migA, migB, migSlow, migFail}},
}

// VariantByTag returns the variant with tag.
func VariantByTag(tag string) (Variant, bool) {
	for _, v := range Variants {
		if v.Tag == tag {
			return v, true
		}
	}
	return Variant{}, false
}

// Release is a built variant on the host.
type Release struct {
	Tag       string
	Dir       string // <workdir>/dist/<tag>
	Binary    string // <Dir>/krill-linux-<arch>
	Checksums string // <Dir>/checksums.txt
	Source    string // <workdir>/src-<tag>, the exported and patched tree
}

// crashAnchor is the code right after the migrations call in cmd/krill/main.go:
// the simulated crash is inserted after it, i.e. once migrations have
// returned successfully.
const crashAnchor = "\tstopMig()\n\tif err != nil {\n\t\treturn err\n\t}\n"

// crashLine is what the crash variant inserts after crashAnchor.
const crashLine = "\treturn errors.New(\"acceptance: simulated crash after migrations\")\n"

// BuildRelease exports HEAD into <workdir>/src-<tag> (the working tree is
// never touched), applies the variant, cross-compiles the server binary
// stamped with the tag, and writes checksums.txt in sha256sum format.
func BuildRelease(ctx context.Context, t testing.TB, cfg Config, v Variant) (Release, error) {
	t.Helper()
	rel := Release{
		Tag:    v.Tag,
		Dir:    filepath.Join(cfg.Workdir, "dist", v.Tag),
		Source: filepath.Join(cfg.Workdir, "src-"+v.Tag),
	}
	rel.Binary = filepath.Join(rel.Dir, cfg.Asset())
	rel.Checksums = filepath.Join(rel.Dir, "checksums.txt")

	for _, dir := range []string{rel.Source, rel.Dir} {
		if err := os.RemoveAll(dir); err != nil {
			return rel, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return rel, err
		}
	}
	export := fmt.Sprintf("git -C %s archive --format=tar HEAD | tar -x -C %s", shQuote(cfg.RepoRoot), shQuote(rel.Source))
	if _, err := runHost(ctx, t, "", nil, "sh", "-c", export); err != nil {
		return rel, fmt.Errorf("exporting HEAD: %w", err)
	}

	migDir := filepath.Join(rel.Source, "internal", "database", "migrations")
	for _, m := range v.Migrations {
		for suffix, body := range map[string]string{".up.sql": m.Up, ".down.sql": m.Down} {
			path := filepath.Join(migDir, m.Name+suffix)
			if _, err := os.Stat(path); err == nil {
				return rel, fmt.Errorf("%s already exists in the exported tree: the branch gained migrations, renumber the variants", path)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return rel, err
			}
		}
		t.Logf("%s: added migration %s", v.Tag, m.Name)
	}
	if v.CrashAfterMigrations {
		if err := patchCrash(filepath.Join(rel.Source, "cmd", "krill", "main.go")); err != nil {
			return rel, fmt.Errorf("%s: %w", v.Tag, err)
		}
		t.Logf("%s: inserted the simulated crash after the migrations call", v.Tag)
	}

	ldflags := "-s -w -X github.com/proshik/krill/internal/buildinfo.Version=" + v.Tag
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+cfg.Arch)
	if _, err := runHost(ctx, t, rel.Source, env, "go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", rel.Binary, "./cmd/krill"); err != nil {
		return rel, fmt.Errorf("building %s: %w", v.Tag, err)
	}

	sum, err := fileSHA256(rel.Binary)
	if err != nil {
		return rel, err
	}
	// Exactly what `sha256sum krill-linux-*` writes in release.yml: the
	// digest, two spaces, the bare asset name.
	line := sum + "  " + cfg.Asset() + "\n"
	if err := os.WriteFile(rel.Checksums, []byte(line), 0o644); err != nil {
		return rel, err
	}
	return rel, nil
}

// patchCrash inserts crashLine after crashAnchor in main.go. It fails when
// the anchor is not found exactly once, when the file does not import
// "errors", or when the inserted line is not found afterwards.
func patchCrash(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	src := string(data)
	if n := strings.Count(src, crashAnchor); n != 1 {
		return fmt.Errorf("crash anchor (stopMig() + err check after RunMigrationsContext) found %d times in %s, want exactly 1", n, path)
	}
	if !strings.Contains(src, "database.RunMigrationsContext(migCtx, cfg.DatabaseURL)\n"+crashAnchor) {
		return fmt.Errorf("crash anchor in %s does not directly follow the RunMigrationsContext call", path)
	}
	if !strings.Contains(src, "\n\t\"errors\"\n") {
		return fmt.Errorf("%s does not import \"errors\"", path)
	}
	patched := strings.Replace(src, crashAnchor, crashAnchor+crashLine, 1)
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		return err
	}
	// Verify with grep, independently of the replace above.
	out, err := exec.Command("grep", "-n", "-A1", "-F", "acceptance: simulated crash after migrations", path).CombinedOutput()
	if err != nil || strings.Count(string(out), "simulated crash after migrations") != 1 {
		return fmt.Errorf("verifying the crash patch in %s: grep found %q (err %v)", path, out, err)
	}
	out, err = exec.Command("grep", "-n", "-B4", "-F", "acceptance: simulated crash after migrations", path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "stopMig()") {
		return fmt.Errorf("verifying the crash patch position in %s: %q (err %v)", path, out, err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runHost runs a command on the Mac, logging it and its output.
func runHost(ctx context.Context, t testing.TB, dir string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	t.Logf("$ %s %s", name, strings.Join(quoteArgs(args), " "))
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	if env != nil {
		c.Env = env
	}
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	start := time.Now()
	err := c.Run()
	out := buf.String()
	t.Logf("  (%s, err=%v)\n%s", time.Since(start).Round(time.Millisecond), err, indent(bounded(out, 4000)))
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, bounded(strings.TrimSpace(out), 2000))
	}
	return out, nil
}

// --- GitHub (Phase B only) ---------------------------------------------------
//
// Everything below WRITES to GitHub. It is called only from
// TestSelfUpdateAcceptance, never from TestVMSmoke.

// PublishRelease creates the GitHub release for rel in cfg.Repo with the
// binary and checksums.txt attached, unless a release with that tag already
// exists. It is created with --latest=false; SetLatest steers "latest".
// The repository must already exist (with at least one commit, so the tag
// has something to point at).
func PublishRelease(ctx context.Context, t testing.TB, cfg Config, rel Release) error {
	t.Helper()
	if _, err := runHost(ctx, t, "", nil, "gh", "repo", "view", cfg.Repo, "--json", "name"); err != nil {
		return fmt.Errorf("test repository %s is not reachable; create it first (public, with a README commit): %w", cfg.Repo, err)
	}
	if _, err := runHost(ctx, t, "", nil, "gh", "release", "view", rel.Tag, "--repo", cfg.Repo, "--json", "tagName"); err == nil {
		t.Logf("release %s already exists in %s; re-uploading its assets", rel.Tag, cfg.Repo)
		_, err := runHost(ctx, t, "", nil, "gh", "release", "upload", rel.Tag, "--repo", cfg.Repo, "--clobber",
			rel.Binary, rel.Checksums)
		return err
	}
	_, err := runHost(ctx, t, "", nil, "gh", "release", "create", rel.Tag, "--repo", cfg.Repo,
		"--title", rel.Tag, "--notes", "Throwaway Krill release for the self-update acceptance test.",
		"--latest=false", rel.Binary, rel.Checksums)
	return err
}

// SetLatest marks tag as the repository's latest release.
func SetLatest(ctx context.Context, t testing.TB, cfg Config, tag string) error {
	t.Helper()
	_, err := runHost(ctx, t, "", nil, "gh", "release", "edit", tag, "--repo", cfg.Repo, "--latest")
	return err
}
