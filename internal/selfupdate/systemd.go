package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Supported reports whether this process can replace its own binary and
// have systemd restart it: Linux, started by systemd (INVOCATION_ID set and
// parented to PID 1), running as root, with a known binary path, and with
// the binary directory, the systemd unit directory (for the drop-in) and the
// run directory (for the markers) all writable. On false the second value is
// a short reason code ("os", "systemd", "root", "binary_path",
// "binary_dir_readonly", "unit_dir_readonly", "run_dir_readonly") — the first
// failing check, in that order.
func (u *Updater) Supported() (bool, string) {
	if ok, reason := u.underSystemd(); !ok {
		return false, reason
	}
	if u.binPath == "" {
		return false, "binary_path"
	}
	// A read-only directory (ProtectSystem= without a ReadWritePaths= for it)
	// only shows up when we actually try to write.
	switch {
	case !dirWritable(filepath.Dir(u.binPath)):
		return false, "binary_dir_readonly"
	case !dirWritable(u.unitDir):
		return false, "unit_dir_readonly"
	case !dirWritable(u.runDir):
		return false, "run_dir_readonly"
	}
	return true, ""
}

// underSystemd reports whether this is a Linux process started by systemd
// (INVOCATION_ID set and parented to PID 1) running as root, with the reason
// codes "os", "systemd" and "root". It is the part of Supported that does not
// probe the filesystem — all ConfirmStartup needs, since confirming never
// writes to the unit directory and only removes files on a best-effort basis.
func (u *Updater) underSystemd() (bool, string) {
	switch {
	case u.goos != "linux":
		return false, "os"
	case u.getenv("INVOCATION_ID") == "" || u.getppid() != 1:
		return false, "systemd"
	case u.geteuid() != 0:
		return false, "root"
	}
	return true, ""
}

// dirWritable creates and removes a probe file in dir.
func dirWritable(dir string) bool {
	probe, err := os.CreateTemp(dir, probePattern)
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// probePattern names Supported's write probes like the other staged files,
// so a leftover one is swept by removeStaged.
const probePattern = ".krill-probe-*.tmp"

// safeUnitName is the conservative subset of systemd unit names accepted
// from the cgroup path. The name is shell-quoted in every command, but it
// still passes through systemd-run into a transient unit's command line and
// through systemctl's argument parser, so backslash escapes (systemd writes
// "-" in an instance name as `\x2d`) and a leading dash (an option to
// systemctl) are not trusted.
var safeUnitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$`)

// unitFromCgroup returns the systemd service unit this process runs in, read
// from the contents of /proc/self/cgroup: the last path segment ending in
// ".service". The cgroup-v2 line ("0::") and the cgroup-v1 "name=systemd"
// hierarchy are preferred over other v1 controllers. It returns "" when no
// such segment is found, or when the name is not safe to put on a command
// line (the caller then falls back to krill.service).
func unitFromCgroup(content string) string {
	fallback := ""
	for _, line := range strings.Split(content, "\n") {
		// hierarchy-ID:controller-list:cgroup-path — the path itself may
		// contain colons, so split into at most three fields.
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		unit := ""
		for _, seg := range strings.Split(fields[2], "/") {
			if strings.HasSuffix(seg, ".service") {
				unit = seg
			}
		}
		if unit == "" {
			continue
		}
		if fields[1] == "" || fields[1] == "name=systemd" {
			return validUnit(unit)
		}
		if fallback == "" {
			fallback = unit
		}
	}
	return validUnit(fallback)
}

func validUnit(unit string) string {
	if !safeUnitName.MatchString(unit) {
		return ""
	}
	return unit
}

// dropInName and dropInContent make a crash-looping new binary keep being
// restarted every RestartSec until the revert timer fires, instead of systemd
// giving up after the default start-rate limit and leaving the unit failed.
// A drop-in survives install.sh rewriting the unit file itself.
const (
	dropInName    = "10-krill-update.conf"
	dropInContent = "[Unit]\nStartLimitIntervalSec=0\n"
)

// writeDropIn makes sure <unitDir>/<unit>.d/10-krill-update.conf holds
// exactly dropInContent. It reports whether it had to write the file — the
// caller then runs `systemctl daemon-reload`.
func writeDropIn(unitDir, unit string) (bool, error) {
	dir := filepath.Join(unitDir, unit+".d")
	path := filepath.Join(dir, dropInName)
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, []byte(dropInContent)) {
		return false, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, []byte(dropInContent), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// stopTimerCmd stops the revert and restart timers and clears the failed
// state of both transient units, so the next systemd-run can reuse the unit
// names. It never fails on its own.
func (u *Updater) stopTimerCmd() string {
	return fmt.Sprintf("systemctl stop %[1]s.timer %[2]s.timer 2>/dev/null; "+
		"systemctl reset-failed %[1]s.timer %[1]s.service %[2]s.timer %[2]s.service 2>/dev/null; true",
		u.revertUnit, u.restartUnit)
}

// armTimerCmd schedules the revert script revertDelay from now.
func (u *Updater) armTimerCmd(tag string) string {
	return fmt.Sprintf("systemd-run --on-active=%d --timer-property=AccuracySec=1s --unit=%s sh -c %s",
		int64(u.revertDelay/time.Second), u.revertUnit, shellQuote(revertScript(u.binPath, u.runDir, u.unit, tag)))
}

// restartCmd schedules the unit's restart from outside its own cgroup: a
// transient timer starts a transient service two seconds from now, and that
// service runs `systemctl restart`. Running systemctl directly would make it
// a child of this process, inside krill.service's cgroup, and the stop half
// of the restart (KillMode=control-group) SIGTERMs it along with us: the
// runner could then report "signal: terminated" for a restart that is
// actually happening, and the job's cleanup would undo a good update while
// the process exits. The runner returns as soon as the timer exists, so an
// error here means nothing was scheduled.
func (u *Updater) restartCmd() string {
	return fmt.Sprintf("systemd-run --on-active=2 --timer-property=AccuracySec=100ms --unit=%s systemctl restart %s",
		u.restartUnit, shellQuote(u.unit))
}

// runCmd runs a host command, folding its output into the error.
func (u *Updater) runCmd(ctx context.Context, cmd string) error {
	out, err := u.runner.Run(ctx, "", cmd)
	if err != nil {
		if msg := strings.TrimSpace(out); msg != "" {
			return fmt.Errorf("%w: %s", err, truncate(msg, 200))
		}
		return err
	}
	return nil
}
