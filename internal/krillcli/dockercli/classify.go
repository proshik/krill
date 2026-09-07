package dockercli

import "strings"

// Hint turns a docker failure into the thing the user should actually do
// about it. Every case here is one that reads as unrelated to its cause:
// "denied" does not say "log in", and "exec format error" does not say
// "your Mac built an arm64 image for an amd64 server".
//
// An unrecognized failure returns "" — docker's own message is passed
// through untouched rather than wrapped in a guess.
func Hint(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "denied") || strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "authentication required"):
		return "the registry refused these credentials — run `docker login <registry>` and try again"

	case strings.Contains(s, "name unknown") || strings.Contains(s, "repository does not exist") ||
		strings.Contains(s, "repository name not known"):
		return "the registry has no such repository — check the spelling in krill.yaml, and create the repository if your registry needs it created first"

	case strings.Contains(s, "no such image"):
		return "the image was built but is not in your local image store, which happens with a buildx `docker-container` builder — add `--load` to your builder setup or switch back to the default `docker` driver"

	case strings.Contains(s, "exec format error"):
		return "the image was built for a different CPU architecture than the server runs — set image.platform in krill.yaml (usually linux/amd64)"

	case strings.Contains(s, "no match for platform") || strings.Contains(s, "does not match the specified platform"):
		return "your docker cannot build for that platform — install emulation with `docker run --privileged --rm tonistiigi/binfmt --install amd64`"

	case strings.Contains(s, "cannot connect to the docker daemon") ||
		strings.Contains(s, "is the docker daemon running"):
		return "docker is not running — start Docker Desktop, or `colima start`"

	case strings.Contains(s, "no space left on device"):
		return "the disk is full — `docker system prune` frees space taken by old build layers"
	}
	return ""
}
