package selfupdate

import (
	"context"
	"errors"
	"testing"
)

// RollbackBinary reports what <bin>.prev is: absent, a release, or unknown —
// unlike Previous, which answers false alike for all three and for a binary
// not older than the running one.
func TestRollbackBinary(t *testing.T) {
	cases := []struct {
		name      string
		noBinPath bool
		noPrev    bool
		out       string
		err       error
		want      RollbackBinary
	}{
		{name: "no krill.prev", noPrev: true, want: RollbackBinary{Kind: RollbackAbsent}},
		{name: "a release", out: "krill v0.16.0\n", want: RollbackBinary{Kind: RollbackRelease, Version: "v0.16.0"}},
		{name: "a release not older than the running one", out: "krill " + testTag + "\n", want: RollbackBinary{Kind: RollbackRelease, Version: testTag}},
		{name: "--version fails", out: "krill v0.16.0\n", err: errors.New("exit status 2"), want: RollbackBinary{Kind: RollbackUnknown}},
		{name: "--version times out", err: context.DeadlineExceeded, want: RollbackBinary{Kind: RollbackUnknown}},
		{name: "no krill vX.Y.Z", out: "krill dev+abc\n", want: RollbackBinary{Kind: RollbackUnknown}},
		{name: "empty output", out: "", want: RollbackBinary{Kind: RollbackUnknown}},
		{name: "binary path unresolved", noBinPath: true, want: RollbackBinary{Kind: RollbackUnknown}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if !tc.noPrev {
				writeFile(t, h.prev, prevContent)
			}
			if tc.noBinPath {
				h.u.binPath = ""
			}
			calls := 0
			h.u.execVersion = func(_ context.Context, path string) (string, error) {
				calls++
				if path != h.prev {
					t.Errorf("exec of %s, want %s", path, h.prev)
				}
				return tc.out, tc.err
			}
			got := h.u.RollbackBinary(context.Background())
			if got != tc.want {
				t.Fatalf("RollbackBinary() = %+v, want %+v", got, tc.want)
			}
			if (tc.noPrev || tc.noBinPath) && calls != 0 {
				t.Errorf("exec calls = %d, want 0", calls)
			}
		})
	}
}

// The parsed version is cached per file, as Previous caches it: page renders
// do not exec the binary every time.
func TestRollbackBinary_Cached(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.prev, prevContent)
	h.set(func() { h.prevOut = "krill v0.16.0\n" })
	for i := 0; i < 2; i++ {
		if got := h.u.RollbackBinary(context.Background()); got.Kind != RollbackRelease || got.Version != "v0.16.0" {
			t.Fatalf("RollbackBinary() = %+v", got)
		}
	}
	h.u.Previous(context.Background())
	if n := h.execCount(); n != 1 {
		t.Errorf("exec calls = %d, want 1", n)
	}
}

// A run the caller cut short says nothing about the file and is not cached.
func TestRollbackBinary_CallerCancelled(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.prev, prevContent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.u.execVersion = func(ctx context.Context, _ string) (string, error) { return "", ctx.Err() }
	if got := h.u.RollbackBinary(ctx); got.Kind != RollbackUnknown {
		t.Fatalf("RollbackBinary() = %+v, want unknown", got)
	}
	h.u.execVersion = h.execVersion
	h.set(func() { h.prevOut = "krill v0.16.0\n" })
	if got := h.u.RollbackBinary(context.Background()); got.Kind != RollbackRelease {
		t.Fatalf("RollbackBinary() after a cancelled run = %+v, want a release", got)
	}
}
