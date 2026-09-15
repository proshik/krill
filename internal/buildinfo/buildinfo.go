// Package buildinfo holds the single build-version identity shared by both
// binaries (cmd/krill and cmd/krill-cli), so the server can report its own
// release the same way the CLI already did.
package buildinfo

import (
	"runtime/debug"

	"golang.org/x/mod/semver"
)

// Version is set by the linker at release time via
// -X github.com/proshik/krill/internal/buildinfo.Version=<tag>. The fallback
// below covers `go install`, `go run`, and a plain `go build` from a checkout.
var Version = "dev"

// String returns the running binary's version: the linker-stamped value when
// present, otherwise the module's build info (so `go install ...@v0.1.0`
// reports its version with no linker flags at all), otherwise a
// "dev+<commit>[-dirty]" string built from VCS info, otherwise the "dev"
// fallback.
func String() string {
	if Version != "dev" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		return "dev+" + rev + dirty
	}
	return Version
}

// IsRelease reports whether v is a release semver tag (valid semver, no
// prerelease suffix) — e.g. "v0.1.0" is a release, "v0.2.0-rc1" and "dev" are
// not.
func IsRelease(v string) bool {
	return semver.IsValid(v) && semver.Prerelease(v) == ""
}
