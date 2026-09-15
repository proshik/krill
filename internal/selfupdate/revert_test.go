package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":             `''`,
		"plain":        `'plain'`,
		"with space":   `'with space'`,
		"it's":         `'it'\''s'`,
		"$HOME;`x`":    "'$HOME;`x`'",
		"''":           `''\'''\'''`,
		"/run/krill-x": `'/run/krill-x'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRevertScript_DefaultPaths(t *testing.T) {
	got := revertScript("/usr/local/bin/krill", "/run", "krill.service", "v0.2.0")
	want := `[ -e '/run/krill-update-pending' ] || exit 0; ` +
		`if [ -e '/usr/local/bin/krill.prev' ]; then mv -f '/usr/local/bin/krill.prev' '/usr/local/bin/krill'; fi; ` +
		`rm -f '/run/krill-update-pending'; ` +
		`echo 'v0.2.0' > '/run/krill-update-reverted'; ` +
		`systemctl reset-failed 'krill.service'; ` +
		`systemctl restart 'krill.service'`
	if got != want {
		t.Errorf("revertScript() =\n%s\nwant\n%s", got, want)
	}
	// systemd may treat % as a specifier in a transient unit's ExecStart.
	if strings.Contains(shellQuote(got), "%") {
		t.Errorf("revert script contains a %% character: %s", got)
	}
}

func TestRevertScript_QuotesPaths(t *testing.T) {
	got := revertScript("/opt/it's here/krill", "/var/run dir", "krill.service", "v1.2.3")
	want := `[ -e '/var/run dir/krill-update-pending' ] || exit 0; ` +
		`if [ -e '/opt/it'\''s here/krill.prev' ]; then mv -f '/opt/it'\''s here/krill.prev' '/opt/it'\''s here/krill'; fi; ` +
		`rm -f '/var/run dir/krill-update-pending'; ` +
		`echo 'v1.2.3' > '/var/run dir/krill-update-reverted'; ` +
		`systemctl reset-failed 'krill.service'; ` +
		`systemctl restart 'krill.service'`
	if got != want {
		t.Errorf("revertScript() =\n%s\nwant\n%s", got, want)
	}
}

// The script travels to systemd-run as a single `sh -c` argument, so the
// whole thing is quoted once more; pin that form too.
func TestRevertScript_QuotedForShell(t *testing.T) {
	got := shellQuote(revertScript("/usr/local/bin/krill", "/run", "krill.service", "v0.2.0"))
	want := `'[ -e '\''/run/krill-update-pending'\'' ] || exit 0; ` +
		`if [ -e '\''/usr/local/bin/krill.prev'\'' ]; then mv -f '\''/usr/local/bin/krill.prev'\'' '\''/usr/local/bin/krill'\''; fi; ` +
		`rm -f '\''/run/krill-update-pending'\''; ` +
		`echo '\''v0.2.0'\'' > '\''/run/krill-update-reverted'\''; ` +
		`systemctl reset-failed '\''krill.service'\''; ` +
		`systemctl restart '\''krill.service'\'''`
	if got != want {
		t.Errorf("shellQuote(revertScript()) =\n%s\nwant\n%s", got, want)
	}
}

// runRevert executes the quoted script the way the timer does (a shell
// running `sh -c <script>`), with a fake systemctl on PATH that logs its
// arguments. It returns what systemctl was asked to do.
func runRevert(t *testing.T, bin, runDir string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	fake := t.TempDir()
	log := filepath.Join(fake, "systemctl.log")
	stub := "#!/bin/sh\necho \"$*\" >> " + shellQuote(log) + "\n"
	if err := os.WriteFile(filepath.Join(fake, "systemctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", "sh -c "+shellQuote(revertScript(bin, runDir, "krill.service", "v0.2.0")))
	cmd.Env = []string{"PATH=" + fake + string(os.PathListSeparator) + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("revert script failed: %v: %s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestRevertScript_RunsInShell(t *testing.T) {
	base := filepath.Join(t.TempDir(), "it's a dir")
	binDir, runDir := filepath.Join(base, "bin"), filepath.Join(base, "run")
	for _, d := range []string{binDir, runDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(binDir, "krill")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	pending := filepath.Join(runDir, pendingMarkerName)
	reverted := filepath.Join(runDir, revertedMarkerName)

	t.Run("no pending marker leaves everything alone", func(t *testing.T) {
		write(bin, "new")
		write(bin+prevSuffix, "old")
		if calls := runRevert(t, bin, runDir); calls != "" {
			t.Errorf("systemctl calls = %q, want none", calls)
		}
		if got := read(bin); got != "new" {
			t.Errorf("bin = %q, want new", got)
		}
		if _, err := os.Stat(reverted); !os.IsNotExist(err) {
			t.Errorf("reverted marker written without a pending marker (stat err %v)", err)
		}
	})

	t.Run("pending marker reverts and restarts", func(t *testing.T) {
		write(bin, "new")
		write(bin+prevSuffix, "old")
		write(pending, "{}")
		calls := runRevert(t, bin, runDir)
		if want := "reset-failed krill.service\nrestart krill.service\n"; calls != want {
			t.Errorf("systemctl calls = %q, want %q", calls, want)
		}
		if got := read(bin); got != "old" {
			t.Errorf("bin = %q, want old", got)
		}
		if _, err := os.Stat(bin + prevSuffix); !os.IsNotExist(err) {
			t.Errorf("prev still present (stat err %v)", err)
		}
		if _, err := os.Stat(pending); !os.IsNotExist(err) {
			t.Errorf("pending marker still present (stat err %v)", err)
		}
		if got := read(reverted); got != "v0.2.0\n" {
			t.Errorf("reverted marker = %q, want %q", got, "v0.2.0\n")
		}
	})

	t.Run("pending marker without prev still restarts", func(t *testing.T) {
		_ = os.Remove(reverted)
		write(bin, "current")
		write(pending, "{}")
		calls := runRevert(t, bin, runDir)
		if !strings.Contains(calls, "restart krill.service") {
			t.Errorf("systemctl calls = %q, want a restart", calls)
		}
		if got := read(bin); got != "current" {
			t.Errorf("bin = %q, want current", got)
		}
	})
}
