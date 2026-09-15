package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// supportedUpdater returns an Updater whose environment passes every
// Supported check, with its binary, unit dir and run dir in fresh temp dirs.
func supportedUpdater(t *testing.T) *Updater {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "krill")
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Updater{
		goos:    "linux",
		getenv:  func(k string) string { return map[string]string{"INVOCATION_ID": "abc123"}[k] },
		getppid: func() int { return 1 },
		geteuid: func() int { return 0 },
		binPath: bin,
		unitDir: t.TempDir(),
		runDir:  t.TempDir(),
	}
}

func TestSupported(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(u *Updater)
		reason string
	}{
		{"ok", func(*Updater) {}, ""},
		{"not linux", func(u *Updater) { u.goos = "darwin" }, "os"},
		{"not linux wins over not root", func(u *Updater) { u.goos = "darwin"; u.geteuid = func() int { return 1000 } }, "os"},
		{"no invocation id", func(u *Updater) { u.getenv = func(string) string { return "" } }, "systemd"},
		{"parent is not pid 1", func(u *Updater) { u.getppid = func() int { return 4242 } }, "systemd"},
		{"not root", func(u *Updater) { u.geteuid = func() int { return 1000 } }, "root"},
		{"no binary path", func(u *Updater) { u.binPath = "" }, "binary_path"},
		{"binary dir not writable", func(u *Updater) {
			u.binPath = filepath.Join(filepath.Dir(u.binPath), "missing", "krill")
		}, "binary_dir_readonly"},
		{"binary dir wins over unit dir", func(u *Updater) {
			u.binPath = filepath.Join(filepath.Dir(u.binPath), "missing", "krill")
			u.unitDir = filepath.Join(u.unitDir, "missing")
		}, "binary_dir_readonly"},
		{"unit dir not writable", func(u *Updater) {
			u.unitDir = filepath.Join(u.unitDir, "missing")
		}, "unit_dir_readonly"},
		{"unit dir wins over run dir", func(u *Updater) {
			u.unitDir = filepath.Join(u.unitDir, "missing")
			u.runDir = filepath.Join(u.runDir, "missing")
		}, "unit_dir_readonly"},
		{"run dir not writable", func(u *Updater) {
			u.runDir = filepath.Join(u.runDir, "missing")
		}, "run_dir_readonly"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := supportedUpdater(t)
			tc.mutate(u)
			ok, reason := u.Supported()
			if ok != (tc.reason == "") || reason != tc.reason {
				t.Errorf("Supported() = (%v, %q), want (%v, %q)", ok, reason, tc.reason == "", tc.reason)
			}
		})
	}
}

func TestSupported_RemovesProbes(t *testing.T) {
	u := supportedUpdater(t)
	if ok, reason := u.Supported(); !ok {
		t.Fatalf("Supported() = false, %q", reason)
	}
	for dir, want := range map[string][]string{
		filepath.Dir(u.binPath): {"krill"},
		u.unitDir:               nil,
		u.runDir:                nil,
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Errorf("%s holds %v, want %v", dir, names, want)
		}
	}
}

func TestUnderSystemd(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(u *Updater)
		reason string
	}{
		{"ok", func(*Updater) {}, ""},
		{"not linux", func(u *Updater) { u.goos = "darwin" }, "os"},
		{"not under systemd", func(u *Updater) { u.getppid = func() int { return 7 } }, "systemd"},
		{"not root", func(u *Updater) { u.geteuid = func() int { return 1000 } }, "root"},
		// The directory checks belong to Supported only.
		{"unwritable dirs do not matter", func(u *Updater) {
			u.binPath = ""
			u.unitDir = filepath.Join(u.unitDir, "missing")
			u.runDir = filepath.Join(u.runDir, "missing")
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := supportedUpdater(t)
			tc.mutate(u)
			ok, reason := u.underSystemd()
			if ok != (tc.reason == "") || reason != tc.reason {
				t.Errorf("underSystemd() = (%v, %q), want (%v, %q)", ok, reason, tc.reason == "", tc.reason)
			}
		})
	}
}

// The probe is named like the other staged files, so one left behind by a
// crash between create and remove is swept on the next start.
func TestSupported_ProbeNameIsSwept(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, probePattern)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if name := filepath.Base(f.Name()); !isStagedName(name) || !strings.HasPrefix(name, ".krill-probe-") {
		t.Errorf("probe name %q, want .krill-probe-*.tmp", name)
	}
	removeStaged(dir)
	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
		t.Errorf("leftover probe not swept (stat err %v)", err)
	}
}

func TestUnitFromCgroup(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"cgroup v2": {"0::/system.slice/krill.service\n", "krill.service"},
		"cgroup v1": {
			"12:pids:/system.slice/krill.service\n" +
				"11:memory:/system.slice/krill.service\n" +
				"2:cpu,cpuacct:/system.slice/krill.service\n" +
				"1:name=systemd:/system.slice/krill.service\n",
			"krill.service",
		},
		"v1 prefers the systemd hierarchy": {
			"3:memory:/\n" +
				"2:cpu:/system.slice/other.service\n" +
				"1:name=systemd:/system.slice/krill.service\n",
			"krill.service",
		},
		"delegated subgroup":          {"0::/system.slice/krill.service/init.scope\n", "krill.service"},
		"nested user service":         {"0::/user.slice/user-1000.slice/user@1000.service/app.slice/krill.service\n", "krill.service"},
		"template instance":           {"0::/system.slice/system-krill.slice/krill@prod.service\n", "krill@prod.service"},
		"session scope":               {"0::/user.slice/user-1000.slice/session-3.scope\n", ""},
		"root cgroup":                 {"0::/\n", ""},
		"empty":                       {"", ""},
		"colon inside the path":       {"0::/system.slice/krill:a.service\n", "krill:a.service"},
		"escaped name is not trusted": {"0::/system.slice/krill\\x2dprod.service\n", ""},
		"leading dash is not trusted": {"0::/system.slice/-krill.service\n", ""},
		"garbage":                     {"not a cgroup file", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := unitFromCgroup(tc.in); got != tc.want {
				t.Errorf("unitFromCgroup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteDropIn(t *testing.T) {
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, "krill.service.d", "10-krill-update.conf")
	const want = "[Unit]\nStartLimitIntervalSec=0\n"

	changed, err := writeDropIn(unitDir, "krill.service")
	if err != nil || !changed {
		t.Fatalf("first writeDropIn() = (%v, %v), want (true, nil)", changed, err)
	}
	assertDropIn := func() {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("drop-in = %q, want %q", data, want)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("drop-in mode = %v, want 0644", fi.Mode().Perm())
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("drop-in dir holds %d entries, want 1 (temp file left behind?)", len(entries))
		}
	}
	assertDropIn()

	changed, err = writeDropIn(unitDir, "krill.service")
	if err != nil || changed {
		t.Fatalf("second writeDropIn() = (%v, %v), want (false, nil)", changed, err)
	}
	assertDropIn()

	if err := os.WriteFile(path, []byte("[Unit]\nStartLimitIntervalSec=10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err = writeDropIn(unitDir, "krill.service")
	if err != nil || !changed {
		t.Fatalf("writeDropIn() over stale content = (%v, %v), want (true, nil)", changed, err)
	}
	assertDropIn()
}
