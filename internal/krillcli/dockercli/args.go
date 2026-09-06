// Package dockercli drives the local docker CLI.
//
// It shells out rather than using the Go SDK, and that is a capability
// decision rather than a dependency one — the SDK is already a direct
// dependency of this module. Two things are only available through the CLI:
// BuildKit, and therefore --platform cross-builds; and ~/.docker/config.json
// credential helpers, which is how a developer on macOS is logged in to a
// registry at all. Reimplementing keychain-backed auth to save a subprocess
// would be a large amount of security-sensitive code for no gain.
//
// Every argv is assembled by a pure function here so it can be asserted in a
// table test without docker installed; runner.go holds the only exec site.
package dockercli

import (
	"fmt"
	"sort"
)

// BuildSpec is one `docker build`.
type BuildSpec struct {
	Ref        string // repository:tag to tag the result with
	Platform   string // e.g. linux/amd64; empty leaves it to docker
	Dockerfile string
	Context    string
	BuildArgs  map[string]string
	NoCache    bool
}

// BuildArgs assembles the argv for a build.
//
// Build args are emitted in sorted key order so the command is byte-identical
// across runs. Go map iteration is randomized, and without the sort a
// --dry-run would print a different command each time and no test could
// assert one.
func BuildArgs(s BuildSpec) []string {
	args := []string{"build"}
	if s.Platform != "" {
		args = append(args, "--platform", s.Platform)
	}
	if s.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, "-t", s.Ref)
	if s.Dockerfile != "" {
		args = append(args, "-f", s.Dockerfile)
	}
	for _, k := range sortedKeys(s.BuildArgs) {
		args = append(args, "--build-arg", k+"="+s.BuildArgs[k])
	}
	ctx := s.Context
	if ctx == "" {
		ctx = "."
	}
	// "--" terminates option parsing: a context path is caller-supplied and
	// one beginning with "-" would otherwise be read as a flag.
	return append(args, "--", ctx)
}

// PushArgs assembles the argv for a push.
func PushArgs(ref string) []string { return []string{"push", "--", ref} }

// InspectIDArgs asks for an image's content id.
//
// Run after every build, because a successful `docker build` does not
// guarantee the image is in the LOCAL image store: with a buildx
// docker-container driver the result stays in the builder unless --load is
// passed, and the next step then fails with a bare "no such image" that names
// neither the cause nor the fix.
func InspectIDArgs(ref string) []string {
	return []string{"image", "inspect", "-f", "{{.Id}}", "--", ref}
}

// SaveArgs streams an image out as a tar.
//
// Saved BY ID, never by reference: `docker save` writes the tags of whatever
// it was given into the archive, and `docker load` on the other end applies
// them. An archive that carries no tags cannot rename anything on the server,
// which is what lets the server name the image itself.
func SaveArgs(imageID string) []string { return []string{"save", "--", imageID} }

// ManifestInspectArgs checks that a reference exists in its registry, using
// the local docker credentials. Used when a push was skipped, so that a tag
// that was never pushed is reported now rather than as a pull failure on the
// server minutes later.
func ManifestInspectArgs(ref string) []string {
	return []string{"manifest", "inspect", "--", ref}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ValidateBuildArgKey rejects a build-arg name docker would not accept, so a
// typo in krill.yaml is reported against that file rather than as a build
// failure.
func ValidateBuildArgKey(k string) error {
	if k == "" {
		return fmt.Errorf("build arg name is empty")
	}
	for i, r := range k {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("invalid build arg name %q: use letters, digits and underscore, not starting with a digit", k)
		}
	}
	return nil
}
