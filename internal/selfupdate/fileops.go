package selfupdate

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// swapWithPrev exchanges the files named bin and prev without a moment in
// which bin is missing: tmp becomes a second name for bin, prev is renamed
// onto bin, tmp onto prev. It returns how far it got — 0: nothing changed
// (tmp may exist), 1: bin holds the old prev and tmp the old bin, prev is
// gone, 2: the exchange is complete.
func swapWithPrev(bin, prev, tmp string) (int, error) {
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err := linkOrCopy(bin, tmp); err != nil {
		return 0, err
	}
	if err := os.Rename(prev, bin); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, prev); err != nil {
		return 1, err
	}
	return 2, syncDir(filepath.Dir(bin))
}

// undoSwap puts bin and prev back after swapWithPrev stopped at stage.
// Best effort: every failure is logged.
func undoSwap(bin, prev, tmp string, stage int) {
	switch stage {
	case 0:
		removeFile(tmp)
	case 1:
		if err := linkOrCopy(bin, prev); err != nil {
			slog.Error("self-update: restoring the previous binary failed", "path", prev, "err", err)
			return
		}
		if err := os.Rename(tmp, bin); err != nil {
			slog.Error("self-update: restoring the running binary failed", "path", bin, "err", err)
			return
		}
		if err := syncDir(filepath.Dir(bin)); err != nil {
			slog.Error("self-update: syncing the binary directory failed", "err", err)
		}
	case 2:
		switch back, err := swapWithPrev(bin, prev, tmp); {
		case err == nil:
		case back == 0:
			removeFile(tmp)
			slog.Error("self-update: swapping the binaries back failed", "err", err)
		default:
			slog.Error("self-update: swapping the binaries back failed", "stage", back, "err", err)
		}
	}
}

// writeFileAtomic replaces path with data: a temp file in the same
// directory, fsynced, chmodded, renamed over path, then the directory
// fsynced. A crash leaves either the old file or the new one, never half.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a rename or create inside it is durable.
// Filesystems that refuse fsync on a directory (EINVAL) are tolerated.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// linkFile is os.Link, replaceable by tests to exercise the copy fallback.
var linkFile = os.Link

// linkOrCopy makes dst a second name for src (a hard link), or — where the
// filesystem refuses links (EXDEV, EPERM) — a durable 0755 copy of it.
func linkOrCopy(src, dst string) error {
	err := linkFile(src, dst)
	if err == nil || !(errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EPERM)) {
		return err
	}
	return copyFile(src, dst)
}

// copyFile copies src into a temp file next to dst (named like the other
// staged files, so a leftover is swept), makes it 0755, fsyncs it and renames
// it over dst: a crash mid-copy never leaves a truncated binary under dst.
// Once the rename succeeded the copy is in place, so a failing directory
// fsync is only logged: callers read an error as "no file at dst".
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	out, err := os.CreateTemp(dir, ".krill-copy-*.tmp")
	if err != nil {
		return err
	}
	tmp := out.Name()
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	// chmod before fsync, so the mode is part of what is made durable.
	if err = out.Chmod(0o755); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, dst); err != nil {
		return err
	}
	if serr := syncDir(dir); serr != nil {
		slog.Warn("self-update: syncing the directory after a copy failed", "dir", dir, "err", serr)
	}
	return nil
}
