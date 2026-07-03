//go:build integration

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// TestVolumeChown seeds a root-owned file into a fresh volume, chowns the volume
// to 1000:1000, and verifies via VolumeArchive that the file's ownership changed.
func TestVolumeChown(t *testing.T) {
	ctx := context.Background()
	eng, err := NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	e := eng.(*dockerEngine)
	vol := "krill-test-chown-vol"
	defer e.VolumeRemove(ctx, vol)

	var seed bytes.Buffer
	writeTar(t, &seed, map[string]string{"file.txt": "hi"})
	if err := e.VolumeRestore(ctx, vol, &seed); err != nil {
		t.Fatalf("seed VolumeRestore: %v", err)
	}

	if err := e.VolumeChown(ctx, vol, 1000, 1000, ""); err != nil {
		t.Fatalf("VolumeChown: %v", err)
	}

	var arch bytes.Buffer
	if err := e.VolumeArchive(ctx, vol, &arch); err != nil {
		t.Fatalf("VolumeArchive: %v", err)
	}
	tr := tar.NewReader(&arch)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg && strings.HasSuffix(hdr.Name, "file.txt") {
			found = true
			if hdr.Uid != 1000 || hdr.Gid != 1000 {
				t.Fatalf("owner = %d:%d, want 1000:1000", hdr.Uid, hdr.Gid)
			}
		}
	}
	if !found {
		t.Fatal("file.txt not found in archive")
	}
}
