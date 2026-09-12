package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/proshik/krill/internal/netguard"
)

// sanitizeGitURL strips any embedded credentials (https://user:token@host/...)
// before the URL is echoed into the deploy log, which is persisted and visible
// to read-only members. The clone itself still uses the original URL.
func sanitizeGitURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// gitAuthSetup, for a private clone, writes a GIT_ASKPASS helper to a temp file
// and returns the env that feeds credentials to git. The credentials are passed
// via environment variables (KRILL_GIT_USERNAME/KRILL_GIT_PASSWORD) that the
// helper echoes on demand — they are NEVER written into the URL, the argv, or
// the helper file itself, so the token cannot leak into the process list or the
// member-visible deploy log. cleanup removes the helper file (always safe to call).
func gitAuthSetup(gitURL string, auth *GitAuth) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	if auth == nil {
		return nil, cleanup, nil
	}
	u, perr := url.Parse(gitURL)
	if perr != nil {
		return nil, cleanup, perr
	}
	if u.Scheme != "https" {
		return nil, cleanup, errors.New("private clone requires an https git_url")
	}
	f, ferr := os.CreateTemp("", "krill-askpass-*.sh")
	if ferr != nil {
		return nil, cleanup, ferr
	}
	name := f.Name()
	cleanup = func() { os.Remove(name) }
	// git calls the askpass helper with the prompt ("Username for ..." /
	// "Password for ...") as $1; answer from the environment.
	const script = "#!/bin/sh\ncase \"$1\" in\n*[Uu]sername*) printf '%s' \"$KRILL_GIT_USERNAME\" ;;\n*) printf '%s' \"$KRILL_GIT_PASSWORD\" ;;\nesac\n"
	// Every failure past this point removes the file itself and hands back a
	// no-op cleanup: callers return early on error, and one that forgets to run
	// cleanup would otherwise leave the helper script behind on every attempt.
	if _, werr := f.WriteString(script); werr != nil {
		f.Close()
		cleanup()
		return nil, func() {}, werr
	}
	if cerr := f.Close(); cerr != nil {
		cleanup()
		return nil, func() {}, cerr
	}
	if cherr := os.Chmod(name, 0o700); cherr != nil {
		cleanup()
		return nil, func() {}, cherr
	}
	env = []string{
		"GIT_ASKPASS=" + name,
		"KRILL_GIT_USERNAME=" + auth.Username,
		"KRILL_GIT_PASSWORD=" + auth.Token,
	}
	return env, cleanup, nil
}

// gitBuilder builds the image: git clone → docker build, via the CLI.
type gitBuilder struct {
	dockerHost   string // value for the child docker's DOCKER_HOST; "" = inherit
	allowPrivate bool   // mirrors KRILL_ALLOW_PRIVATE_EGRESS for the git_url netguard check
}

func (b *gitBuilder) Build(ctx context.Context, req BuildRequest, out io.Writer) error {
	if err := ValidateBuildRequest(req); err != nil {
		fmt.Fprintf(out, "❌ invalid build request: %v\n", err)
		return err
	}

	// git_url goes through the same SSRF egress guard as S3 and registry URLs:
	// without it, a tenant could point an app at an internal/loopback address
	// and use the build worker to probe the private network. HostOf keeps a
	// non-standard port (needed elsewhere to compare against a stored
	// credential host), but net.LookupIPAddr — which CheckHost calls — errors
	// on a "host:port" string, so the port is stripped just for this call.
	host, err := HostOf(req.GitURL)
	if err != nil {
		return err
	}
	if err := netguard.CheckHost(ctx, bareHost(host), b.allowPrivate); err != nil {
		return fmt.Errorf("git_url host %q is not allowed: %w", host, err)
	}

	dir, err := os.MkdirTemp("", fmt.Sprintf("krill-build-%d-%d-", req.AppID, req.DeployID))
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	branch := req.GitBranch
	if branch == "" {
		branch = "main"
	}
	dockerfile := req.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}

	// Private clones authenticate via a GIT_ASKPASS helper, NOT a token embedded
	// in the clone URL: that keeps the token out of argv (the process list) and
	// out of the deploy log (which read-only members can view), where git's own
	// error output could otherwise surface it.
	authEnv, authCleanup, err := gitAuthSetup(req.GitURL, req.GitAuth)
	if err != nil {
		authCleanup() // belt and braces: gitAuthSetup already cleans up its own failures
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	}
	defer authCleanup()
	fmt.Fprintf(out, "→ git clone %s (branch %s)\n", sanitizeGitURL(req.GitURL), branch)
	gitEnv := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=http:https")
	gitEnv = append(gitEnv, authEnv...)
	if err := b.run(ctx, out, "git", cloneArgs(req.GitURL, branch, dir), gitEnv); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	dfPath := filepath.Join(dir, dockerfile)
	cdir := contextDir(dir, dockerfile)

	// Validate build-arg keys and write each build secret to a 0600 temp file in a
	// dir OUTSIDE the build context (docker build never ingests it); wiped on return.
	for k := range req.BuildArgs {
		if !validBuildKey(k) {
			return fmt.Errorf("invalid build arg key: %q", k)
		}
	}
	secretFiles := map[string]string{}
	if len(req.BuildSecrets) > 0 {
		sdir, serr := os.MkdirTemp("", "krill-secrets-")
		if serr != nil {
			return serr
		}
		defer os.RemoveAll(sdir)
		for k, v := range req.BuildSecrets {
			if !validBuildKey(k) {
				return fmt.Errorf("invalid build secret key: %q", k)
			}
			p := filepath.Join(sdir, k)
			if werr := os.WriteFile(p, []byte(v), 0o600); werr != nil {
				return werr
			}
			secretFiles[k] = p
		}
	}

	cacheNote := ""
	if req.NoCache {
		cacheNote = " --no-cache"
	}
	fmt.Fprintf(out, "→ docker build%s -t %s -f %s %s\n", cacheNote, req.ImageTag, dfPath, cdir)
	env := append(os.Environ(), "DOCKER_BUILDKIT=1")
	if b.dockerHost != "" {
		env = append(env, "DOCKER_HOST="+b.dockerHost)
	}
	if err := b.run(ctx, out, "docker", buildArgs(req.ImageTag, dfPath, cdir, req.NoCache, req.BuildArgs, secretFiles), env); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}
	fmt.Fprintf(out, "✅ build complete: %s\n", req.ImageTag)
	return nil
}

func (b *gitBuilder) run(ctx context.Context, out io.Writer, name string, args []string, env []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if env != nil {
		cmd.Env = env
	}
	return cmd.Run()
}

// Available checks that git and docker are present in PATH.
func Available() error {
	for _, bin := range []string{"git", "docker"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found in PATH: %w", bin, err)
		}
	}
	return nil
}
