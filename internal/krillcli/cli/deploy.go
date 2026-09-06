package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/proshik/krill/internal/krillcli/deployflow"
	"github.com/proshik/krill/internal/krillcli/dockercli"
	"github.com/proshik/krill/internal/krillcli/gitmeta"
	"github.com/proshik/krill/internal/krillcli/project"
	"github.com/spf13/cobra"
)

func newDeployCmd() *cobra.Command {
	var (
		app        string
		tag        string
		platform   string
		useUpload  bool
		useReg     bool
		skipBuild  bool
		skipPush   bool
		noWatch    bool
		dryRun     bool
		noCache    bool
		reqClean   bool
		allowMism  bool
		noWaitLock bool
		timeout    time.Duration
		buildArgs  []string
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

Exit codes: 0 ok · 1 the deployment failed · 2 configuration · 3 another
deploy was in flight · 4 timed out watching · 5 deployed but not running ·
6 authentication.`,
		Args: cobra.NoArgs,
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

			if tag == "" {
				tag, err = deriveTag(cmd.Context(), p, reqClean)
				if err != nil {
					return usageErr(err)
				}
			}

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
				Dockerfile:         relDockerfile(p),
				Context:            p.BuildContextPath(),
				BuildArgs:          merged,
				Delivery:           delivery,
				SkipBuild:          skipBuild,
				SkipPush:           skipPush,
				NoWatch:            noWatch,
				DryRun:             dryRun,
				NoCache:            noCache,
				AllowImageMismatch: allowMism,
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
	f.BoolVar(&noWaitLock, "no-wait-for-lock", false, "exit instead of waiting for another deployment to finish")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the rollout")
	f.StringArrayVar(&buildArgs, "build-arg", nil, "extra build argument, KEY=VALUE (repeatable)")
	return cmd
}

func deriveTag(ctx context.Context, p *project.Config, requireClean bool) (string, error) {
	info := gitmeta.Info{}
	if gitmeta.Available() {
		var err error
		info, err = gitmeta.Describe(ctx, p.Dir)
		if err != nil {
			return "", err
		}
	}
	if !info.InRepo && p.Tag.Strategy == "git" {
		fmt.Fprintln(os.Stderr, "warning: not a git repository — falling back to a timestamp tag")
	}
	cfg := gitmeta.TagConfig{
		Strategy:     p.Tag.Strategy,
		Prefix:       p.Tag.Prefix,
		RequireClean: p.Tag.RequireClean || requireClean,
	}
	tag, err := gitmeta.DeriveTag(info, cfg, time.Now())
	if err != nil {
		var dirty *gitmeta.ErrDirty
		if errors.As(err, &dirty) {
			return "", fmt.Errorf("%w — commit or stash first, or drop --require-clean", err)
		}
		return "", err
	}
	if info.Dirty {
		fmt.Fprintf(os.Stderr, "warning: %d uncommitted change(s); the tag is marked -dirty and identifies no commit\n", info.Modified)
	}
	return tag, nil
}

// relDockerfile expresses the Dockerfile relative to the build context, which
// is what `docker build -f` expects when the context is passed separately.
func relDockerfile(p *project.Config) string {
	rel, err := filepath.Rel(p.BuildContextPath(), p.DockerfilePath())
	if err != nil {
		return p.DockerfilePath()
	}
	return rel
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
