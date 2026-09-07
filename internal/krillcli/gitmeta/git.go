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
	//
	// The `-- .` pathspec scopes the answer to dir's subtree rather than to
	// the whole repository. Without it, a monorepo holding one service per
	// directory reports every service's edits as this one's: editing
	// services/web marks a deploy of services/bot dirty, tags the image
	// -dirty, and refuses the build outright under require_clean — for changes
	// that are not in the build context and cannot reach the image.
	if out, err := git(ctx, dir, "status", "--porcelain", "--", "."); err == nil && out != "" {
		lines := strings.Split(out, "\n")
		info.Dirty = true
		info.Modified = len(lines)
		// Kept for the warning: "3 uncommitted changes" sends the reader to
		// `git status`, whereas naming the files usually ends the question on
		// the spot — most often it is one generated or untracked file.
		for _, l := range lines {
			if p := porcelainPath(l); p != "" {
				info.DirtyPaths = append(info.DirtyPaths, p)
			}
			if len(info.DirtyPaths) == maxDirtyPaths {
				break
			}
		}
	}
	return info, nil
}

// maxDirtyPaths caps how many paths the warning names.
const maxDirtyPaths = 3

// porcelainPath takes the path out of one `git status --porcelain` line.
//
// It cannot use a fixed offset. The format is two status columns and a space,
// but the common " M path" case begins with a space, and git() trims the whole
// output — which eats that leading column on the FIRST line only, so a fixed
// slice loses a character from exactly one of the paths it prints.
func porcelainPath(line string) string {
	l := strings.TrimSpace(line)
	i := strings.IndexByte(l, ' ')
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(l[i+1:])
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
