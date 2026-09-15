//go:build linux || darwin

package selfupdate

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// While a copy is in progress the destination name must not exist: the bytes
// go to a staged temp file that is renamed into place only once complete. A
// FIFO as the source holds the copy open halfway.
func TestCopyFile_NothingUnderTheRealNameMidCopy(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.fifo"), filepath.Join(dir, "krill.prev")
	if err := syscall.Mkfifo(src, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- copyFile(src, dst) }()

	w, err := os.OpenFile(src, os.O_WRONLY, 0) // blocks until copyFile opens it
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("half-")); err != nil {
		t.Fatal(err)
	}

	// Wait for the first half to land in the staged file.
	deadline := time.Now().Add(5 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(dir, ".krill-copy-*.tmp"))
		if len(matches) == 1 {
			if fi, err := os.Stat(matches[0]); err == nil && fi.Size() == int64(len("half-")) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the copy never reached its staged file")
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertMissing(t, dst)

	if _, err := w.Write([]byte("done")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("copyFile() error = %v", err)
	}
	assertContent(t, dst, "half-done")
	assertNoStaged(t, dir)
}

// Once the copy is renamed into place the file exists: a directory fsync
// that fails afterwards must not be reported as a failed copy, or callers
// would treat a present file as missing. A write+search-only directory lets
// CreateTemp and Rename succeed but makes opening it for the fsync fail.
func TestCopyFile_DirSyncFailureAfterRename(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src, dst := filepath.Join(srcDir, "krill"), filepath.Join(dstDir, "krill.prev")
	writeFile(t, src, oldContent)
	if err := os.Chmod(dstDir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dstDir, 0o700) })
	if err := syncDir(dstDir); err == nil {
		t.Skip("this platform opens a directory without read permission")
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile() error = %v, want nil once the file is in place", err)
	}
	if err := os.Chmod(dstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertContent(t, dst, oldContent)
	assertNoStaged(t, dstDir)
}
