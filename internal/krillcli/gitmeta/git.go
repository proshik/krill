package gitmeta

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// Available reports whether git is on PATH. Checked up front so a missing git
// is reported as a precondition rather than as a confusing failure partway
// through a deploy.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// Describe reads the state of the working tree at dir.
//
// A directory that is not a git repository is not an error: the caller falls
// back to a timestamp tag. Somebody building from an unpacked tarball should
// get a working deploy and a warning, not a refusal.
func Describe(ctx context.Context, dir string) (Info, error) {
	if _, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return Info{InRepo: false}, nil
	}

	info := Info{InRepo: true}

	sha, err := git(ctx, dir, "rev-parse", "--short="+strconv.Itoa(ShortSHALen), "HEAD")
	if err != nil {
		// A repository with no commits yet. Same fallback as no repository at
		// all — there is nothing to name a build after.
		return Info{InRepo: false}, nil
	}
	info.SHA = sha

	// "HEAD" here means a detached head, not a branch called HEAD.
	if branch, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && branch != "HEAD" {
		info.Branch = branch
	}

	// --porcelain is the stable, script-facing format; the human one is not
	// guaranteed across git versions or locales.
	if out, err := git(ctx, dir, "status", "--porcelain"); err == nil && out != "" {
		info.Dirty = true
		info.Modified = len(strings.Split(out, "\n"))
	}
	return info, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Never let git stop for credentials or a pager: this runs unattended
	// inside a deploy, and a prompt would hang it with no visible cause.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
