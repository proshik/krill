//go:build acceptance

// Package acceptance is the live acceptance harness for Krill's self-update:
// it builds throwaway release variants of the current branch, installs Krill
// into a disposable Lima VM with install.sh and drives Settings -> Updates
// over HTTP, asserting on the VM's real systemd, binary and schema state.
//
// Nothing here runs unless KRILL_ACCEPT=1 and the build tag `acceptance` is
// set; see docs/development.md ("Self-update acceptance").
package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds the harness settings, read from the environment.
type Config struct {
	// VM is the Lima instance name (KRILL_ACCEPT_VM). The harness never
	// touches any other Lima instance.
	VM string
	// Repo is the throwaway GitHub repository ("owner/name") the test
	// releases are published to and Krill updates from (KRILL_ACCEPT_REPO).
	Repo string
	// HostPort is the macOS port forwarded to the VM's :8080
	// (KRILL_ACCEPT_HOST_PORT).
	HostPort int
	// KeepVM leaves the VM behind after the run (KRILL_ACCEPT_KEEP_VM=1).
	KeepVM bool
	// Workdir is git-ignored scratch space: exported sources, built
	// releases, the Lima YAML and report.md (KRILL_ACCEPT_WORKDIR).
	Workdir string
	// Arch is the GOARCH of the VM and of the release binaries
	// (KRILL_ACCEPT_ARCH).
	Arch string
	// RepoRoot is the top of the Krill git checkout.
	RepoRoot string
}

// LoadConfig reads the harness settings from the environment, applies the
// defaults and creates the workdir.
func LoadConfig() (Config, error) {
	root, err := repoRoot()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		VM:       envOr("KRILL_ACCEPT_VM", "krill-accept"),
		Repo:     envOr("KRILL_ACCEPT_REPO", "proshik/krill-update-e2e"),
		KeepVM:   os.Getenv("KRILL_ACCEPT_KEEP_VM") == "1",
		Workdir:  envOr("KRILL_ACCEPT_WORKDIR", filepath.Join(root, ".superpowers", "acceptance")),
		Arch:     envOr("KRILL_ACCEPT_ARCH", "arm64"),
		RepoRoot: root,
	}
	port, err := strconv.Atoi(envOr("KRILL_ACCEPT_HOST_PORT", "28080"))
	if err != nil || port < 1024 || port > 65535 {
		return Config{}, fmt.Errorf("KRILL_ACCEPT_HOST_PORT: want a port in 1024..65535, got %q", os.Getenv("KRILL_ACCEPT_HOST_PORT"))
	}
	cfg.HostPort = port
	if cfg.Arch != "arm64" && cfg.Arch != "amd64" {
		return Config{}, fmt.Errorf("KRILL_ACCEPT_ARCH: want arm64 or amd64, got %q", cfg.Arch)
	}
	if strings.Count(cfg.Repo, "/") != 1 {
		return Config{}, fmt.Errorf("KRILL_ACCEPT_REPO: want owner/name, got %q", cfg.Repo)
	}
	if !filepath.IsAbs(cfg.Workdir) {
		cfg.Workdir = filepath.Join(root, cfg.Workdir)
	}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		return Config{}, fmt.Errorf("creating the workdir: %w", err)
	}
	return cfg, nil
}

// LimaArch is Cfg.Arch in Lima's spelling.
func (c Config) LimaArch() string {
	if c.Arch == "amd64" {
		return "x86_64"
	}
	return "aarch64"
}

// Asset is the release asset name of the server binary.
func (c Config) Asset() string {
	return "krill-linux-" + c.Arch
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("locating the repository root: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
