package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/proshik/krill/internal/krillcli/deployflow"
	"github.com/proshik/krill/internal/krillcli/dockercli"
	"github.com/proshik/krill/internal/krillcli/gitmeta"
	"github.com/proshik/krill/internal/krillcli/project"
	"github.com/spf13/cobra"
)

func newDeployCmd() *cobra.Command {
	var (
		app          string
		tag          string
		platform     string
		useUpload    bool
		useReg       bool
		skipBuild    bool
		skipPush     bool
		noWatch      bool
		dryRun       bool
		noCache      bool
		reqClean     bool
		allowMism    bool
		allowMissing bool
		noWaitLock   bool
		timeout      time.Duration
		buildArgs    []string
	)

	cmd := &cobra.Command{
		Use:     "deploy",
		Aliases: []string{"up"},
		Short:   "Build this project, ship the image, and deploy it",
		Long: `Build the image from this directory, get it to the server, deploy it, and
wait for the rollout.

Every check that can fail happens before the build: the token's level, that
the application exists and is an image app, and that krill.yaml's repository
is the one Krill actually pulls from. A four-minute build followed by a
rejection is the failure this ordering exists to prevent.

To deploy an image that is already in the registry, pass --skip-build
--skip-push with the tag you want.

If another deployment is already running, this waits for it and then deploys
once. Pass --no-wait-for-lock to exit with code 3 instead.

Exit codes: 0 ok · 1 the deployment failed · 2 configuration · 3 another
deploy was in flight (--no-wait-for-lock) · 4 timed out watching, or the
outcome could not be read · 5 deployed but not running · 6 authentication.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !dockercli.Available() {
				return usageErr(fmt.Errorf("docker is not on PATH — krill-cli builds the image locally, so it needs one"))
			}
			s, err := connect(true)
			if err != nil {
				return err
			}
			p := s.proj

			ref := p.App
			if app != "" {
				ref = app
			}
			if platform == "" {
				platform = p.Image.Platform
			}
			delivery := p.Delivery
			switch {
			case useUpload && useReg:
				return usageErr(errors.New("--upload and --registry contradict each other"))
			case useUpload:
				delivery = project.DeliveryUpload
			case useReg:
				delivery = project.DeliveryRegistry
			}

			// Run for BOTH branches. --tag used to skip this entirely, which
			// meant it also skipped tag.require_clean and the dirty warning:
			// a team that set require_clean got an uncommitted tree built and
			// shipped under a clean-looking tag, which is the one thing that
			// setting exists to prevent.
			info, err := gitState(cmd.Context(), p)
			if err != nil {
				return usageErr(err)
			}
			if err := enforceClean(info, p, reqClean); err != nil {
				return usageErr(err)
			}
			if tag == "" {
				tag, err = deriveTag(info, p)
				if err != nil {
					return usageErr(err)
				}
			}
			warnDirty(info)

			// Checked here rather than inside the flow so the message can
			// name the file and the resolved path.
			if !skipBuild {
				if _, err := os.Stat(p.DockerfilePath()); err != nil {
					return usageErr(fmt.Errorf("no Dockerfile at %s (build.dockerfile in %s)", p.DockerfilePath(), p.Path))
				}
			}

			extra, err := parseBuildArgs(buildArgs)
			if err != nil {
				return usageErr(err)
			}
			merged := map[string]string{}
			for k, v := range p.Build.Args {
				merged[k] = v
			}
			for k, v := range extra {
				merged[k] = v
			}

			flow := &deployflow.Flow{API: s.api, Docker: dockercli.Exec{}, Out: os.Stdout}
			return flow.Run(cmd.Context(), deployflow.Options{
				Ref:                ref,
				Repository:         p.Image.Repository,
				Platform:           platform,
				Tag:                tag,
				Dockerfile:         p.DockerfilePath(),
				Context:            p.BuildContextPath(),
				BuildArgs:          merged,
				Delivery:           delivery,
				SkipBuild:          skipBuild,
				SkipPush:           skipPush,
				NoWatch:            noWatch,
				DryRun:             dryRun,
				NoCache:            noCache,
				AllowImageMismatch: allowMism,
				AllowMissingImage:  allowMissing,
				NoWaitForLock:      noWaitLock,
				CanWriteHint:       s.resolved.CanWriteHint(),
				Timeout:            timeout,
			})
		},
	}

	f := cmd.Flags()
	f.StringVarP(&app, "app", "a", "", "application to deploy (default: `app` in krill.yaml)")
	f.StringVarP(&tag, "tag", "t", "", "image tag (default: derived from git)")
	f.StringVar(&platform, "platform", "", "build platform (default: image.platform in krill.yaml)")
	f.BoolVar(&useUpload, "upload", false, "stream the image to Krill instead of pushing to a registry")
	f.BoolVar(&useReg, "registry", false, "push to a registry (the default)")
	f.BoolVar(&skipBuild, "skip-build", false, "do not build; the image must already exist")
	f.BoolVar(&skipPush, "skip-push", false, "do not push; the tag must already be in the registry")
	f.BoolVar(&noWatch, "no-watch", false, "return as soon as the deployment is queued")
	f.BoolVar(&dryRun, "dry-run", false, "print exactly what would run, and stop")
	f.BoolVar(&noCache, "no-cache", false, "build without using cached layers")
	f.BoolVar(&reqClean, "require-clean", false, "refuse to build with uncommitted changes")
	f.BoolVar(&allowMism, "allow-image-mismatch", false, "proceed even if krill.yaml and Krill disagree about the repository")
	f.BoolVar(&allowMissing, "allow-missing-image", false, "with --skip-push, proceed even if the tag is not found in the registry")
	f.BoolVar(&noWaitLock, "no-wait-for-lock", false, "exit instead of waiting for another deployment to finish")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the rollout")
	f.StringArrayVar(&buildArgs, "build-arg", nil, "extra build argument, KEY=VALUE (repeatable)")
	return cmd
}

// gitState reads the working tree, scoped to the BUILD CONTEXT rather than to
// the repository root: what makes a build reproducible is the state of the
// files that go into the image, and in a monorepo the rest of the tree does
// not.
func gitState(ctx context.Context, p *project.Config) (gitmeta.Info, error) {
	if !gitmeta.Available() {
		return gitmeta.Info{}, nil
	}
	return gitmeta.Describe(ctx, p.BuildContextPath())
}

// enforceClean applies tag.require_clean regardless of where the tag came
// from.
func enforceClean(info gitmeta.Info, p *project.Config, requireClean bool) error {
	if !(p.Tag.RequireClean || requireClean) || !info.Dirty {
		return nil
	}
	return fmt.Errorf("%w — commit or stash first, or drop --require-clean%s",
		&gitmeta.ErrDirty{Modified: info.Modified}, dirtyPathsSuffix(info))
}

func deriveTag(info gitmeta.Info, p *project.Config) (string, error) {
	if !info.InRepo && p.Tag.Strategy == "git" {
		fmt.Fprintln(os.Stderr, "warning: not a git repository — falling back to a timestamp tag")
	}
	cfg := gitmeta.TagConfig{
		Strategy: p.Tag.Strategy,
		Prefix:   p.Tag.Prefix,
		// Already enforced by enforceClean, which runs for an explicit --tag
		// too; leaving it on here would report the same refusal twice with
		// different wording.
		RequireClean: false,
	}
	tag, err := gitmeta.DeriveTag(info, cfg, time.Now())
	if err != nil {
		var dirty *gitmeta.ErrDirty
		if errors.As(err, &dirty) {
			return "", fmt.Errorf("%w — commit or stash first, or drop --require-clean", err)
		}
		return "", err
	}
	return tag, nil
}

func warnDirty(info gitmeta.Info) {
	if !info.Dirty {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %d uncommitted change(s); the tag identifies no commit%s\n",
		info.Modified, dirtyPathsSuffix(info))
}

// dirtyPathsSuffix names a few of the offending files. "3 uncommitted changes"
// sends the reader to `git status`; naming them usually ends the question —
// most often it is one generated or untracked file, sometimes krill.yaml
// itself right after `krill-cli init`.
func dirtyPathsSuffix(info gitmeta.Info) string {
	if len(info.DirtyPaths) == 0 {
		return ""
	}
	more := ""
	if info.Modified > len(info.DirtyPaths) {
		more = ", …"
	}
	return " (" + strings.Join(info.DirtyPaths, ", ") + more + ")"
}

func parseBuildArgs(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range pairs {
		k, v, ok := cutOne(kv, '=')
		if !ok {
			return nil, fmt.Errorf("--build-arg %q must be KEY=VALUE", kv)
		}
		if err := dockercli.ValidateBuildArgKey(k); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func cutOne(s string, sep byte) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func exitCodeFor(err error) int { return deployflow.CodeOf(err) }
