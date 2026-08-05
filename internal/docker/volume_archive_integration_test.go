//go:build integration

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"testing"
)

// writeTar writes files (path->content) as a tar stream to w.
func writeTar(t *testing.T, w io.Writer, files map[string]string) {
	t.Helper()
	tw := tar.NewWriter(w)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar Write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
}

// tarFiles reads a tar stream from r and returns regular files as path->content.
func tarFiles(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatalf("tar read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = buf.String()
	}
	return out
}

func TestVolumeArchiveRestoreRoundtrip(t *testing.T) {
	eng, err := NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	e, ok := eng.(*dockerEngine)
	if !ok {
		t.Fatalf("expected *dockerEngine, got %T", eng)
	}
	ctx := context.Background()

	const srcVol = "krill-test-vol-src"
	const dstVol = "krill-test-vol-dst"
	t.Cleanup(func() {
		_ = e.VolumeRemove(context.Background(), srcVol)
		_ = e.VolumeRemove(context.Background(), dstVol)
	})

	want := map[string]string{"hello.txt": "world", "sub/a.txt": "AAA"}

	// Seed srcVol by restoring an in-process tar into it.
	var seed bytes.Buffer
	writeTar(t, &seed, want)
	if err := e.VolumeRestore(ctx, srcVol, &seed, ""); err != nil {
		t.Fatalf("VolumeRestore(src): %v", err)
	}

	// Archive srcVol to a buffer.
	var archived bytes.Buffer
	if err := e.VolumeArchive(ctx, srcVol, &archived, ""); err != nil {
		t.Fatalf("VolumeArchive(src): %v", err)
	}

	// Restore the archive into dstVol.
	if err := e.VolumeRestore(ctx, dstVol, bytes.NewReader(archived.Bytes()), ""); err != nil {
		t.Fatalf("VolumeRestore(dst): %v", err)
	}

	// Archive dstVol and assert the files survived the roundtrip.
	var roundtrip bytes.Buffer
	if err := e.VolumeArchive(ctx, dstVol, &roundtrip, ""); err != nil {
		t.Fatalf("VolumeArchive(dst): %v", err)
	}
	got := tarFiles(t, &roundtrip)
	for name, content := range want {
		// tar from `tar -c -C /vol .` prefixes entries with "./".
		v, ok := got[name]
		if !ok {
			v, ok = got["./"+name]
		}
		if !ok {
			t.Fatalf("file %q missing after roundtrip; got keys %v", name, keysOf(got))
		}
		if v != content {
			t.Fatalf("file %q = %q, want %q", name, v, content)
		}
	}
}

// A restore must reproduce the backup, not merge into whatever is there. tar -x
// only overwrites paths present in the archive, so a file created after the
// backup survived the restore — resurrecting state the point-in-time snapshot
// does not contain (stale indexes, lock files, deleted uploads).
func TestVolumeRestoreReplacesExistingContent(t *testing.T) {
	eng, err := NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	e, ok := eng.(*dockerEngine)
	if !ok {
		t.Fatalf("expected *dockerEngine, got %T", eng)
	}
	ctx := context.Background()

	const vol = "krill-test-vol-replace"
	t.Cleanup(func() { _ = e.VolumeRemove(context.Background(), vol) })

	// Seed the volume with the "current" state: one file from the backup plus a
	// stray file (and a stray dotfile) that the backup does not contain.
	var seed bytes.Buffer
	writeTar(t, &seed, map[string]string{
		"keep.txt":  "old",
		"stray.txt": "must not survive",
		".hidden":   "must not survive either",
	})
	if err := e.VolumeRestore(ctx, vol, &seed, ""); err != nil {
		t.Fatalf("VolumeRestore(seed): %v", err)
	}

	// Restore a backup that contains only keep.txt.
	var archive bytes.Buffer
	writeTar(t, &archive, map[string]string{"keep.txt": "restored"})
	if err := e.VolumeRestore(ctx, vol, &archive, ""); err != nil {
		t.Fatalf("VolumeRestore(archive): %v", err)
	}

	var after bytes.Buffer
	if err := e.VolumeArchive(ctx, vol, &after, ""); err != nil {
		t.Fatalf("VolumeArchive: %v", err)
	}
	got := tarFiles(t, &after)
	lookup := func(name string) (string, bool) {
		if v, ok := got[name]; ok {
			return v, true
		}
		v, ok := got["./"+name]
		return v, ok
	}
	if v, ok := lookup("keep.txt"); !ok || v != "restored" {
		t.Errorf("keep.txt = %q (present=%v), want %q", v, ok, "restored")
	}
	if _, ok := lookup("stray.txt"); ok {
		t.Errorf("stray.txt survived the restore: got keys %v", keysOf(got))
	}
	if _, ok := lookup(".hidden"); ok {
		t.Errorf("dotfile .hidden survived the restore: got keys %v", keysOf(got))
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
