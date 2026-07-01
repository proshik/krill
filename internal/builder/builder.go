package builder

import (
	"context"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GitAuth carries HTTPS credentials for cloning a private repo.
type GitAuth struct {
	Username string
	Token    string
}

// BuildRequest — parameters for building an image from git+Dockerfile.
type BuildRequest struct {
	AppID          int64
	DeployID       int64
	GitURL         string
	GitBranch      string
	DockerfilePath string // relative to the repo root
	ImageTag       string // e.g. krill-7:42
	NoCache        bool   // pass --no-cache to docker build (Rebuild)

	GitAuth      *GitAuth          // nil = public clone
	BuildArgs    map[string]string // non-secret --build-arg
	BuildSecrets map[string]string // BuildKit --secret (value = the literal secret)
}

var buildKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validBuildKey checks a build-arg/secret key is a valid identifier (no spaces,
// '=', or ',') so it cannot inject into a docker flag.
func validBuildKey(k string) bool { return buildKeyRe.MatchString(k) }

// sortedKeys returns the map keys in deterministic order (for stable argv).
func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Builder clones the repository and builds the image, streaming output to out.
type Builder interface {
	Build(ctx context.Context, req BuildRequest, out io.Writer) error
}

// ValidateBuildRequest validates user input before running git/docker.
func ValidateBuildRequest(req BuildRequest) error {
	u, err := url.Parse(req.GitURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("git_url must be an http(s) URL")
	}
	if strings.HasPrefix(req.GitBranch, "-") {
		return errors.New("git_branch must not start with '-'")
	}
	df := req.DockerfilePath
	if df != "" {
		if filepath.IsAbs(df) || strings.HasPrefix(df, "-") {
			return errors.New("dockerfile_path must be a relative path not starting with '-'")
		}
		for _, seg := range strings.Split(filepath.ToSlash(filepath.Clean(df)), "/") {
			if seg == ".." {
				return errors.New("dockerfile_path must not escape the build context")
			}
		}
	}
	return nil
}

// cloneArgs — argv for git clone (shallow clone of a single branch).
func cloneArgs(gitURL, branch, dir string) []string {
	return []string{"clone", "--branch", branch, "--depth", "1", "--", gitURL, dir}
}

// buildArgs — argv for docker build. noCache adds --no-cache (forced rebuild);
// bargs become --build-arg K=V; secretFiles become --secret id=K,src=path. Keys
// are emitted in sorted order for deterministic argv.
func buildArgs(tag, dockerfile, context string, noCache bool, bargs, secretFiles map[string]string) []string {
	args := []string{"build"}
	if noCache {
		args = append(args, "--no-cache")
	}
	for _, k := range sortedKeys(bargs) {
		args = append(args, "--build-arg", k+"="+bargs[k])
	}
	for _, k := range sortedKeys(secretFiles) {
		args = append(args, "--secret", "id="+k+",src="+secretFiles[k])
	}
	return append(args, "-t", tag, "-f", dockerfile, context)
}

// contextDir — the build context: the directory containing the Dockerfile.
func contextDir(repoDir, dockerfilePath string) string {
	return filepath.Join(repoDir, filepath.Dir(dockerfilePath))
}
