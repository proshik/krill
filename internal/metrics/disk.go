package metrics

import (
	"bufio"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DiskUsage is one filesystem of a node, as df reports it.
type DiskUsage struct {
	Mount      string // mount point ("/", "/var/lib/docker")
	Filesystem string // device or source, for the operator's orientation only
	Total      int64  // bytes
	Used       int64  // bytes
	Avail      int64  // bytes available to unprivileged users
}

// UsedPct is the share of the filesystem in use exactly as df's Use% shows
// it: used / (used + available), so blocks reserved for root count as full,
// rounded up to a whole percent — the number an operator sees running df on
// the node is the one thresholds are compared with.
func (d DiskUsage) UsedPct() float64 {
	if d.Used+d.Avail <= 0 {
		return 0
	}
	return math.Ceil(float64(d.Used) * 100 / float64(d.Used+d.Avail))
}

// NodeDisk is a stored disk snapshot.
type NodeDisk struct {
	Node string
	DiskUsage
	SampledAt time.Time
}

// DiskSource is the optional capability of a NodeStatsSource to report the
// usage of the filesystems holding the given paths.
type DiskSource interface {
	DiskUsage(ctx context.Context, paths []string) ([]DiskUsage, error)
}

// RunFunc runs argv on a node and returns its standard output.
type RunFunc func(ctx context.Context, argv []string) ([]byte, error)

// DiskViaDF reports disk usage by running `df -P -k <paths>` through run.
// df exits non-zero when one of the paths is missing but still prints the
// others, so its output is used whenever it holds at least one filesystem.
// Paths on the same filesystem collapse into one entry (the first path's).
func DiskViaDF(ctx context.Context, run RunFunc, paths []string) ([]DiskUsage, error) {
	argv := append([]string{"df", "-P", "-k"}, paths...)
	out, err := run(ctx, argv)
	disks := ParseDF(string(out))
	if len(disks) == 0 {
		if err == nil {
			err = errors.New("df printed no filesystems")
		}
		return nil, err
	}
	return disks, nil
}

// ParseDF parses POSIX `df -P -k` output (1024-byte blocks), skipping the
// header and any line it cannot read, and keeps one entry per mount point.
func ParseDF(out string) []DiskUsage {
	var disks []DiskUsage
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if strings.HasPrefix(strings.TrimSpace(line), "Filesystem") {
				continue
			}
		}
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		// The mount point is everything after the capacity column, so a mount
		// path with spaces survives; the filesystem is the first field.
		total, e1 := strconv.ParseInt(f[1], 10, 64)
		used, e2 := strconv.ParseInt(f[2], 10, 64)
		avail, e3 := strconv.ParseInt(f[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || !strings.HasSuffix(f[4], "%") {
			continue
		}
		mount := strings.Join(f[5:], " ")
		if seen[mount] {
			continue
		}
		seen[mount] = true
		disks = append(disks, DiskUsage{
			Mount: mount, Filesystem: f[0],
			Total: total * 1024, Used: used * 1024, Avail: avail * 1024,
		})
	}
	return disks
}

// LocalRun runs argv on this host (the control-plane node: Krill is a host
// binary there).
func LocalRun(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// ShellQuote quotes s for a POSIX shell, for running argv over SSH.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// diskPaths returns the paths whose filesystems a node's snapshot covers: the
// root and the Docker data directory (when it lives elsewhere).
func diskPaths(dockerRoot string) []string {
	paths := []string{"/"}
	if dockerRoot != "" && dockerRoot != "/" && strings.HasPrefix(dockerRoot, "/") {
		paths = append(paths, dockerRoot)
	}
	return paths
}
