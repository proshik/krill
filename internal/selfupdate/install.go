package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxChecksumsBytes = 64 << 10
	maxAssetBytes     = 256 << 20
	// freeSpaceMargin is kept free on top of the binary itself.
	freeSpaceMargin = 16 << 20
	// cleanupTimeout bounds the failure cleanup, which runs on a fresh
	// context because the job's own may be what expired.
	cleanupTimeout = 90 * time.Second
)

// install is the update job. The rename of the staged binary onto binPath is
// the point of no return: a failure before it leaves the installation as it
// was, a failure after it (the restart) puts the old binary back. Both are
// handled by the one deferred cleanup below.
func (u *Updater) install(ctx context.Context, tag string) (err error) {
	bin := u.binPath
	dir := filepath.Dir(bin)
	prev := bin + prevSuffix
	tmp := filepath.Join(dir, ".krill-"+tag+".tmp")
	pending := u.markerPath(pendingMarkerName)
	asset := "krill-linux-" + u.goarch
	rel := strings.TrimRight(u.baseURL, "/") + "/" + u.repo + "/releases/download/" + tag + "/"

	var timerArmed, markerWritten, prevLinked, swapped bool
	defer func() {
		if err == nil {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if swapped {
			if rerr := os.Rename(prev, bin); rerr != nil {
				// Leave the timer and the marker alone: the revert script is
				// now the only thing that can still put the old binary back.
				slog.Error("self-update: restoring the previous binary failed; leaving the revert timer armed",
					"path", bin, "err", rerr)
				return
			}
			if serr := syncDir(dir); serr != nil {
				slog.Error("self-update: syncing the binary directory failed", "dir", dir, "err", serr)
			}
		} else {
			removeFile(tmp)
			if prevLinked {
				// Only a second name for the running binary; keeping it would
				// offer a "rollback" to the version already running.
				removeFile(prev)
			}
		}
		if timerArmed {
			if serr := u.runCmd(cctx, u.stopTimerCmd()); serr != nil {
				slog.Error("self-update: cancelling the revert timer failed", "unit", u.revertUnit, "err", serr)
			}
		}
		if markerWritten {
			removeFile(pending)
		}
	}()

	// 1. A revert or restart unit left from an earlier attempt would make
	// systemd-run refuse the unit name; staged files from a dead job are
	// garbage.
	if err := u.runCmd(ctx, u.stopTimerCmd()); err != nil {
		return fmt.Errorf("clearing old update timers: %w", err)
	}
	removeStaged(dir)

	// 2. The checksum listing, which must name our asset.
	sums, err := u.fetchChecksums(ctx, rel+"checksums.txt")
	if err != nil {
		return err
	}
	want, ok := sums[asset]
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %s", asset)
	}

	// 3-4. The binary, staged next to the installed one.
	got, err := u.download(ctx, rel+asset, asset, tmp)
	if err != nil {
		return err
	}
	u.setPhase(PhaseVerifying)
	if got != want {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("making the new binary executable: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}

	// 5. It must start and report exactly the release we asked for.
	if err := u.smokeTest(ctx, tmp, tag); err != nil {
		return err
	}

	// Start checked for work a restart would cut short, but the download
	// took a while and a deploy or a restore may have begun since. This is
	// the last moment nothing on the host has changed yet, so ask again.
	if reason := u.busyReason(); reason != "" {
		return &BusyError{Reason: reason}
	}

	// 6. Keep a crash-looping new binary restarting until the timer fires.
	u.setPhase(PhaseInstalling)
	changed, err := writeDropIn(u.unitDir, u.unit)
	if err != nil {
		return fmt.Errorf("writing the systemd drop-in: %w", err)
	}
	if changed {
		if err := u.runCmd(ctx, "systemctl daemon-reload"); err != nil {
			return fmt.Errorf("reloading systemd: %w", err)
		}
	}

	// 7. The dead-man switch. Counted as armed from the attempt on: a
	// runner that timed out may still have created the unit.
	timerArmed = true
	if err := u.runCmd(ctx, u.armTimerCmd(tag)); err != nil {
		return fmt.Errorf("arming the revert timer: %w", err)
	}

	// 8. The marker goes down before the swap: should this process die right
	// after the rename, systemd starts the new binary, which finds the marker
	// and confirms itself instead of being reverted.
	markerWritten = true
	if err := writeJSONMarker(pending, pendingMarker{
		From:      u.current,
		To:        tag,
		StartedAt: u.Status().StartedAt.UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("writing the pending marker: %w", err)
	}

	// 9. The swap. A hard link keeps the running inode as .prev with no
	// moment in which bin is missing, and no ETXTBSY from writing over it.
	if err := os.Remove(prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the old %s: %w", prev, err)
	}
	if err := linkOrCopy(bin, prev); err != nil {
		return fmt.Errorf("keeping the running binary as %s: %w", prev, err)
	}
	prevLinked = true
	if err := os.Rename(tmp, bin); err != nil {
		return fmt.Errorf("installing the new binary: %w", err)
	}
	swapped = true
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}

	// 10. Schedule the restart into the new binary from outside our cgroup
	// (see restartCmd); SIGTERM follows a couple of seconds later.
	u.setPhase(PhaseRestarting)
	if err := u.runCmd(ctx, u.restartCmd()); err != nil {
		return fmt.Errorf("scheduling the restart of %s: %w", u.unit, err)
	}
	slog.Info("self-update installed, restart scheduled", "from", u.current, "to", tag, "unit", u.unit)
	return nil
}

// rollback is the Rollback job: swap bin and .prev, leave a marker for the
// next process, restart. A failure puts both names back where they were.
func (u *Updater) rollback(ctx context.Context, version string) (err error) {
	bin := u.binPath
	prev := bin + prevSuffix
	tmp := filepath.Join(filepath.Dir(bin), ".krill-rollback.tmp")
	marker := u.markerPath(rolledbackMarkerName)

	stage := 0
	markerWritten, restartRequested := false, false
	defer func() {
		if err == nil {
			return
		}
		if restartRequested {
			// Nothing should be scheduled after an error, but a runner that
			// timed out may still have created the unit: cancel it before
			// the binaries move back.
			cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			if serr := u.runCmd(cctx, u.stopTimerCmd()); serr != nil {
				slog.Error("self-update: cancelling the restart timer failed", "unit", u.restartUnit, "err", serr)
			}
		}
		undoSwap(bin, prev, tmp, stage)
		if markerWritten {
			removeFile(marker)
		}
	}()

	// A restart unit left from an earlier attempt would make systemd-run
	// refuse the unit name.
	if err := u.runCmd(ctx, u.stopTimerCmd()); err != nil {
		return fmt.Errorf("clearing old update timers: %w", err)
	}
	// Work may have started since Rollback checked; the swap is the first
	// change, so this is the last moment to back out cleanly.
	if reason := u.busyReason(); reason != "" {
		return &BusyError{Reason: reason}
	}
	stage, err = swapWithPrev(bin, prev, tmp)
	if err != nil {
		return fmt.Errorf("swapping in the previous binary: %w", err)
	}
	markerWritten = true
	if err := writeJSONMarker(marker, rolledbackMarker{From: u.current, To: version}); err != nil {
		return fmt.Errorf("writing the rollback marker: %w", err)
	}
	u.setPhase(PhaseRestarting)
	restartRequested = true
	if err := u.runCmd(ctx, u.restartCmd()); err != nil {
		return fmt.Errorf("scheduling the restart of %s: %w", u.unit, err)
	}
	slog.Info("self-update rollback installed, restart scheduled", "from", u.current, "to", version, "unit", u.unit)
	return nil
}

// get performs a GET that must answer 200. Errors name the requested file,
// not the URL a redirect led to.
func (u *Updater) get(ctx context.Context, rawURL, name string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request for %s: %w", name, err)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("krill/%s (%s/%s)", u.current, u.goos, u.goarch))
	resp, err := u.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyDrain))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("downloading %s: unexpected status %d", name, resp.StatusCode)
	}
	return resp, nil
}

// fetchChecksums downloads and parses the release's checksums.txt.
func (u *Updater) fetchChecksums(ctx context.Context, rawURL string) (map[string]string, error) {
	resp, err := u.get(ctx, rawURL, "checksums.txt")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("downloading checksums.txt: %w", err)
	}
	if len(data) > maxChecksumsBytes {
		return nil, fmt.Errorf("checksums.txt is larger than %d bytes", maxChecksumsBytes)
	}
	return parseChecksums(data)
}

// download streams the release asset into tmp and returns its sha256.
func (u *Updater) download(ctx context.Context, rawURL, asset, tmp string) (string, error) {
	resp, err := u.get(ctx, rawURL, asset)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxAssetBytes {
		return "", fmt.Errorf("%s is too large: %d bytes", asset, resp.ContentLength)
	}
	if resp.ContentLength > 0 {
		dir := filepath.Dir(tmp)
		free, err := u.freeBytes(dir)
		if err != nil {
			return "", fmt.Errorf("checking free space in %s: %w", dir, err)
		}
		if need := uint64(resp.ContentLength) + freeSpaceMargin; free < need {
			return "", fmt.Errorf("not enough free space in %s: %d bytes needed, %d available", dir, need, free)
		}
	}
	sum, err := streamToFile(resp.Body, tmp, maxAssetBytes)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", asset, err)
	}
	return sum, nil
}

// streamToFile writes at most limit bytes from r into a new file at path
// (which must not exist), fsyncs it and returns the content's sha256. On
// error the caller removes whatever was written.
func streamToFile(r io.Reader, path string, limit int64) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("too large: over %d bytes", limit)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// smokeTest runs the staged binary's --version and insists on "krill <tag>".
// It catches a binary for another architecture, one that does not start at
// all, and an intact binary of the wrong release.
func (u *Updater) smokeTest(ctx context.Context, path, tag string) error {
	out, err := u.execVersion(ctx, path)
	if err != nil {
		return fmt.Errorf("smoke test: the new binary did not run: %w (output %q)", err, truncate(strings.TrimSpace(out), 200))
	}
	if want := "krill " + tag; firstLine(out) != want {
		return fmt.Errorf("smoke test: the new binary printed %q, want %q", truncate(strings.TrimSpace(out), 200), want)
	}
	return nil
}

// runVersion is the default smoke test: run `<path> --version` with an empty
// environment (the candidate binary must never see the DSN or passwords this
// process holds), from "/", with a 10s deadline and at most 4 KiB of output
// kept.
func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = []string{}
	cmd.Dir = "/"
	out := &boundedBuffer{max: 4 << 10}
	cmd.Stdout = out
	cmd.Stderr = out
	// A child that forks a grandchild holding the output pipe must not keep
	// the smoke test blocked past its deadline.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return string(out.buf), err
}

// boundedBuffer keeps the first max bytes written to it and silently drops
// the rest (still reporting a full write, so the child never sees EPIPE).
type boundedBuffer struct {
	buf []byte
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
