package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Marker files in runDir (a tmpfs, so a reboot clears them together with the
// transient revert timer they coordinate with).
const (
	// pendingMarkerName exists from just before the binary swap until the new
	// process confirms its startup. The revert script does nothing without it.
	pendingMarkerName = "krill-update-pending"
	// revertedMarkerName is written by the revert script: the tag it undid.
	revertedMarkerName = "krill-update-reverted"
	// rolledbackMarkerName is written by Rollback before it restarts.
	rolledbackMarkerName = "krill-update-rolledback"
)

// prevSuffix names the kept previous binary next to the installed one.
const prevSuffix = ".prev"

// maxMarkerBytes bounds how much of a marker file is ever read.
const maxMarkerBytes = 64 << 10

// pendingMarker is the JSON content of the pending marker.
type pendingMarker struct {
	From      string `json:"from"`
	To        string `json:"to"`
	StartedAt string `json:"started_at"`
}

// rolledbackMarker is the JSON content of the rolledback marker.
type rolledbackMarker struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// prevVersionCache remembers the version Previous parsed out of <bin>.prev,
// keyed by the file's identity, size and modification time.
type prevVersionCache struct {
	info    os.FileInfo
	version string
	ok      bool
}

// matches reports whether fi still describes the file the cache was filled
// from.
func (c prevVersionCache) matches(fi os.FileInfo) bool {
	return c.info != nil && os.SameFile(c.info, fi) &&
		c.info.Size() == fi.Size() && c.info.ModTime().Equal(fi.ModTime())
}

func (u *Updater) markerPath(name string) string {
	return filepath.Join(u.runDir, name)
}

// writeJSONMarker atomically writes v as JSON to path, readable by root only.
func writeJSONMarker(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

// readMarker returns the content of a marker file, at most maxMarkerBytes of
// it; a missing file is an error wrapping fs.ErrNotExist.
func readMarker(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMarkerBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxMarkerBytes)
	}
	return data, nil
}

// readJSONMarker decodes a JSON marker file into v.
func readJSONMarker(path string, v any) error {
	data, err := readMarker(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

// markerExists reports whether path exists. Anything but a clean "does not
// exist" counts as present, so a refusal fails closed.
func markerExists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// removeFile removes path, treating "already gone" as success and logging
// any other failure.
func removeFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("self-update: removing a file failed", "path", path, "err", err)
	}
}

// isStagedName matches the temporary files the updater creates next to the
// binary: .krill-<tag>.tmp while installing, .krill-rollback.tmp while
// rolling back.
func isStagedName(name string) bool {
	return strings.HasPrefix(name, ".krill-") && strings.HasSuffix(name, ".tmp")
}

// removeStaged removes leftover staged files from dir — whatever a job that
// died halfway left behind.
func removeStaged(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Error("self-update: listing the binary directory failed", "dir", dir, "err", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() && isStagedName(e.Name()) {
			removeFile(filepath.Join(dir, e.Name()))
		}
	}
}
