// Package imageref compares and validates Docker image references the way the
// CLI needs them: is the repository I am about to push to the same one Krill
// is configured to pull from, and is this tag one both docker and Krill will
// accept.
//
// Both questions are answered before anything is built, because both have the
// same failure mode otherwise — a four-minute build followed by a rejection
// that was knowable up front.
package imageref

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/distribution/reference"
)

// tagRe is the INTERSECTION of two rules that do not agree.
//
// Krill accepts ^[A-Za-z0-9._-]+$ up to 128 characters (webhook.ValidTag).
// Docker additionally forbids a leading '.' or '-', its grammar being
// [\w][\w.-]{0,127}. So a branch called ".wip" or "-hotfix" produces a tag
// Krill would happily store and docker refuses to build, and the rejection
// arrives from `docker build` minutes later rather than from a local check.
//
// Validating against the intersection means a tag this package accepts is
// always one BOTH will take. imageref_test.go asserts that direction against
// webhook.ValidTag directly, so the two cannot drift apart silently.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// MaxTagLen is the longest tag either side accepts.
const MaxTagLen = 128

// ValidTag reports whether tag is acceptable to docker and to Krill alike.
func ValidTag(tag string) bool {
	return tag != "" && len(tag) <= MaxTagLen && tagRe.MatchString(tag)
}

// NormalizeRepo reduces a repository reference to the canonical form docker
// itself would resolve it to, so that two spellings of one repository compare
// equal. Any tag or digest is discarded — this answers "which repository",
// not "which image".
//
// The normalization is not cosmetic and is the reason this delegates to
// distribution/reference rather than trimming strings: "nginx",
// "library/nginx", "docker.io/library/nginx" and "index.docker.io/library/nginx"
// are all one repository, and only Docker Hub gets the implicit "library/"
// namespace. Hand-rolling that is where a mismatch check quietly starts
// producing false alarms.
func NormalizeRepo(ref string) (string, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return "", fmt.Errorf("image reference is empty")
	}
	named, err := reference.ParseNormalizedNamed(trimmed)
	if err != nil {
		return "", fmt.Errorf("not a valid image reference: %q", ref)
	}
	return reference.TrimNamed(named).Name(), nil
}

// SameRepo reports whether two references name the same repository. A
// reference that does not parse is not equal to anything, including another
// unparseable one — "both broken" is not a match.
func SameRepo(a, b string) bool {
	na, err := NormalizeRepo(a)
	if err != nil {
		return false
	}
	nb, err := NormalizeRepo(b)
	if err != nil {
		return false
	}
	return na == nb
}

// Join builds the "repository:tag" reference to build, push and deploy.
//
// It returns the FAMILIAR spelling, not the canonical one NormalizeRepo
// compares: both are valid input to docker, but "myapp:v1" is what the user
// typed and what they expect to read back in the output, whereas the
// canonical "docker.io/library/myapp:v1" looks like the CLI silently pointed
// the build somewhere else. Comparison and display want different forms; only
// comparison needs the canonical one.
func Join(repo, tag string) (string, error) {
	trimmed := strings.TrimSpace(repo)
	named, err := reference.ParseNormalizedNamed(trimmed)
	if err != nil {
		return "", fmt.Errorf("not a valid image reference: %q", repo)
	}
	if !ValidTag(tag) {
		return "", fmt.Errorf("invalid tag %q", tag)
	}
	return reference.FamiliarName(reference.TrimNamed(named)) + ":" + tag, nil
}
