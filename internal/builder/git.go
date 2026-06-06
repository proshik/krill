package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// gitBuilder builds the image: git clone → docker build, via the CLI.
type gitBuilder struct {
	dockerHost string // value for the child docker's DOCKER_HOST; "" = inherit
}

// New creates a Builder. dockerHost is passed into docker build (for the Colima socket).
func New(dockerHost string) Builder { return &gitBuilder{dockerHost: dockerHost} }

func (b *gitBuilder) Build(ctx context.Context, req BuildRequest, out io.Writer) error {
	if err := ValidateBuildRequest(req); err != nil {
		fmt.Fprintf(out, "❌ invalid build request: %v\n", err)
		return err
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

	fmt.Fprintf(out, "→ git clone %s (branch %s)\n", req.GitURL, branch)
	gitEnv := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=http:https")
	if err := b.run(ctx, out, "git", cloneArgs(req.GitURL, branch, dir), gitEnv); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	dfPath := filepath.Join(dir, dockerfile)
	cdir := contextDir(dir, dockerfile)
	cacheNote := ""
	if req.NoCache {
		cacheNote = " --no-cache"
	}
	fmt.Fprintf(out, "→ docker build%s -t %s -f %s %s\n", cacheNote, req.ImageTag, dfPath, cdir)
	var env []string
	if b.dockerHost != "" {
		env = append(os.Environ(), "DOCKER_HOST="+b.dockerHost)
	}
	if err := b.run(ctx, out, "docker", buildArgs(req.ImageTag, dfPath, cdir, req.NoCache), env); err != nil {
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
