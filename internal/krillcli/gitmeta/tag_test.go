package gitmeta_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/gitmeta"
	"github.com/proshik/krill/internal/krillcli/imageref"
)

var fixedNow = time.Date(2026, 9, 6, 14, 15, 30, 0, time.UTC)

func TestDeriveTag(t *testing.T) {
	tests := []struct {
		name string
		info gitmeta.Info
		cfg  gitmeta.TagConfig
		want string
	}{
		{
			name: "clean branch",
			info: gitmeta.Info{InRepo: true, Branch: "main", SHA: "a1b2c3d4e5f6"},
			want: "main-a1b2c3d4e5f6",
		},
		{
			name: "prefix",
			info: gitmeta.Info{InRepo: true, Branch: "main", SHA: "a1b2c3d4e5f6"},
			cfg:  gitmeta.TagConfig{Prefix: "v"},
			want: "vmain-a1b2c3d4e5f6",
		},
		{
			name: "slash in branch",
			info: gitmeta.Info{InRepo: true, Branch: "feature/new-ui", SHA: "a1b2c3d4e5f6"},
			want: "feature-new-ui-a1b2c3d4e5f6",
		},
		{
			// A dirty tree must not reuse one tag for two different trees, so
			// the time is part of it.
			name: "dirty carries a time",
			info: gitmeta.Info{InRepo: true, Branch: "main", SHA: "a1b2c3d4e5f6", Dirty: true, Modified: 3},
			want: "main-a1b2c3d4e5f6-dirty-141530",
		},
		{
			name: "detached head",
			info: gitmeta.Info{InRepo: true, Branch: "", SHA: "a1b2c3d4e5f6"},
			want: "detached-a1b2c3d4e5f6",
		},
		{
			name: "not a repo falls back to a timestamp",
			info: gitmeta.Info{InRepo: false},
			want: "build-20260906-141530",
		},
		{
			name: "timestamp strategy ignores git state",
			info: gitmeta.Info{InRepo: true, Branch: "main", SHA: "a1b2c3d4e5f6"},
			cfg:  gitmeta.TagConfig{Strategy: "timestamp"},
			want: "build-20260906-141530",
		},
		{
			// A branch that sanitizes to nothing must still produce a tag.
			name: "branch of only separators",
			info: gitmeta.Info{InRepo: true, Branch: "---", SHA: "a1b2c3d4e5f6"},
			want: "detached-a1b2c3d4e5f6",
		},
		{
			// Docker forbids a leading dot; Krill would accept it. The
			// sanitizer, not the caller, is what keeps the build from failing.
			name: "branch starting with a dot",
			info: gitmeta.Info{InRepo: true, Branch: ".wip", SHA: "a1b2c3d4e5f6"},
			want: "wip-a1b2c3d4e5f6",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gitmeta.DeriveTag(tc.info, tc.cfg, fixedNow)
			if err != nil {
				t.Fatalf("DeriveTag: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DeriveTag = %q, want %q", got, tc.want)
			}
			if !imageref.ValidTag(got) {
				t.Fatalf("derived tag %q is not a valid image tag", got)
			}
		})
	}
}

// TestDeriveTagTruncatesTheBranchNotTheHash pins which half is expendable. A
// tag cut from the right would lose the commit hash, leaving a tag that names
// a branch and no particular build.
func TestDeriveTagTruncatesTheBranchNotTheHash(t *testing.T) {
	info := gitmeta.Info{InRepo: true, Branch: strings.Repeat("x", 400), SHA: "a1b2c3d4e5f6"}
	got, err := gitmeta.DeriveTag(info, gitmeta.TagConfig{}, fixedNow)
	if err != nil {
		t.Fatalf("DeriveTag: %v", err)
	}
	if len(got) > imageref.MaxTagLen {
		t.Fatalf("tag is %d characters, over the %d limit: %q", len(got), imageref.MaxTagLen, got)
	}
	if !strings.HasSuffix(got, "-a1b2c3d4e5f6") {
		t.Fatalf("commit hash was truncated away: %q", got)
	}
	if !imageref.ValidTag(got) {
		t.Fatalf("truncated tag is invalid: %q", got)
	}
}

func TestDeriveTagRequireClean(t *testing.T) {
	info := gitmeta.Info{InRepo: true, Branch: "main", SHA: "a1b2c3d4e5f6", Dirty: true, Modified: 2}

	if _, err := gitmeta.DeriveTag(info, gitmeta.TagConfig{RequireClean: true}, fixedNow); err == nil {
		t.Fatal("want an error with require_clean and a dirty tree")
	} else {
		var de *gitmeta.ErrDirty
		if !errors.As(err, &de) {
			t.Fatalf("want ErrDirty, got %T: %v", err, err)
		}
		if de.Modified != 2 {
			t.Fatalf("ErrDirty should carry the file count, got %d", de.Modified)
		}
	}

	// Without the flag the same tree builds, just with a marked tag.
	if _, err := gitmeta.DeriveTag(info, gitmeta.TagConfig{}, fixedNow); err != nil {
		t.Fatalf("a dirty tree must still build by default: %v", err)
	}
}

func TestDeriveTagRejectsAnImpossiblePrefix(t *testing.T) {
	info := gitmeta.Info{InRepo: true, Branch: "main", SHA: "a1b2c3d4e5f6"}
	cfg := gitmeta.TagConfig{Prefix: strings.Repeat("p", 200)}
	if _, err := gitmeta.DeriveTag(info, cfg, fixedNow); err == nil {
		t.Fatal("want an error when the prefix alone exceeds the tag limit")
	}
}

func TestSanitizeBranch(t *testing.T) {
	tests := []struct{ in, want string }{
		{"main", "main"},
		{"feature/new-ui", "feature-new-ui"},
		{"release/1.0", "release-1.0"},
		{"a//b", "a-b"},
		{".wip", "wip"},
		{"-hotfix", "hotfix"},
		{"user@host", "user-host"},
		{"---", ""},
		{"", ""},
		{"пример", ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := gitmeta.SanitizeBranch(tc.in); got != tc.want {
				t.Fatalf("SanitizeBranch(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
