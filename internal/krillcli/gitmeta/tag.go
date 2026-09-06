// Package gitmeta turns the state of a git working tree into the image tag a
// deploy will use. The derivation is a pure function of an Info value so it
// can be tested without a repository; only Describe shells out to git.
package gitmeta

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/proshik/krill/internal/krillcli/imageref"
)

// ShortSHALen is how much of the commit hash goes into the tag.
//
// Twelve, not the conventional seven. A short-hash collision in a busy
// repository means deploying a different commit than the one asked for, and
// the cost of finding that out in production dwarfs five characters of tag
// length. Git itself lengthens abbreviations as repositories grow for the
// same reason.
const ShortSHALen = 12

// Info is the state of the working tree the tag is derived from.
type Info struct {
	// InRepo is false when the command ran outside a git work tree at all.
	InRepo bool
	// Branch is the current branch, empty when detached or not in a repo.
	Branch string
	// SHA is the abbreviated commit hash, empty when not in a repo.
	SHA string
	// Dirty reports uncommitted changes; Modified counts the files.
	Dirty    bool
	Modified int
}

// TagConfig is the krill.yaml `tag:` block.
type TagConfig struct {
	// Strategy is "git" (default) or "timestamp".
	Strategy string
	// Prefix is prepended to every derived tag, e.g. "v".
	Prefix string
	// RequireClean refuses to derive a tag from a dirty tree.
	RequireClean bool
}

// ErrDirty is returned when the tree has uncommitted changes and the project
// asked for clean-only builds.
type ErrDirty struct{ Modified int }

func (e *ErrDirty) Error() string {
	return fmt.Sprintf("working tree has %d uncommitted change(s) and tag.require_clean is set", e.Modified)
}

// unsafeRe matches every character a docker tag may not contain.
var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// leadingRe matches the characters a docker tag may not START with, even
// though it may contain them later.
var leadingRe = regexp.MustCompile(`^[^A-Za-z0-9_]+`)

// SanitizeBranch maps a branch name onto the tag charset. Slashes and other
// separators become "-", runs collapse, and any leading "." or "-" is dropped
// because docker rejects a tag beginning with either.
//
// It is lossy on purpose: "release/1.0" and "release-1.0" both become
// "release-1.0". That collision is acceptable only because the commit hash is
// also in the tag, which is what actually identifies the build.
func SanitizeBranch(branch string) string {
	s := unsafeRe.ReplaceAllString(branch, "-")
	s = regexp.MustCompile(`-{2,}`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	s = leadingRe.ReplaceAllString(s, "")
	return s
}

// DeriveTag builds the tag for one build.
//
// Shapes:
//
//	clean      <prefix><branch>-<sha>
//	dirty      <prefix><branch>-<sha>-dirty-<HHMMSS>
//	detached   <prefix>detached-<sha>
//	no repo    <prefix>build-<YYYYMMDD-HHMMSS>
//
// The dirty suffix carries a time, not a bare "-dirty". A bare marker would
// be reused across two different working trees, so two genuinely different
// images would land under one tag — and since the server digest-pins at
// deploy time, which of them actually runs would come down to push ordering.
//
// Truncation, when the result would exceed the tag limit, eats into the
// BRANCH and never the hash: the branch is a label for humans, the hash is
// the identity of the build.
func DeriveTag(info Info, cfg TagConfig, now time.Time) (string, error) {
	if cfg.RequireClean && info.Dirty {
		return "", &ErrDirty{Modified: info.Modified}
	}

	if cfg.Strategy == "timestamp" || !info.InRepo || info.SHA == "" {
		return finish(cfg.Prefix, "build", "-"+now.UTC().Format("20060102-150405"))
	}

	base := SanitizeBranch(info.Branch)
	if base == "" {
		base = "detached"
	}

	suffix := "-" + info.SHA
	if info.Dirty {
		suffix += "-dirty-" + now.UTC().Format("150405")
	}
	return finish(cfg.Prefix, base, suffix)
}

// finish assembles prefix + base + suffix, trimming base to fit and checking
// the result is a tag both docker and Krill accept.
func finish(prefix, base, suffix string) (string, error) {
	budget := imageref.MaxTagLen - len(prefix) - len(suffix)
	if budget < 1 {
		return "", fmt.Errorf("tag prefix %q leaves no room for a tag within %d characters", prefix, imageref.MaxTagLen)
	}
	if len(base) > budget {
		base = strings.Trim(base[:budget], "-.")
	}
	if base == "" {
		base = "build"
	}
	tag := prefix + base + suffix
	if !imageref.ValidTag(tag) {
		return "", fmt.Errorf("derived tag %q is not a valid image tag; check tag.prefix in krill.yaml", tag)
	}
	return tag, nil
}
