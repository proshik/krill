package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/project"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var appRef string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a krill.yaml for this project",
		Long: `Write a krill.yaml in the current directory.

The image repository is read from the application's own configuration in
Krill rather than typed, because a repository that does not match the one
Krill pulls from is the single most confusing failure in this workflow: the
push succeeds, the deploy goes green, and the old image keeps running.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dest := filepath.Join(".", project.FileName)
			if _, err := os.Stat(dest); err == nil && !force {
				return fmt.Errorf("%s already exists here; pass --force to overwrite it", dest)
			}
			s, err := connect(false)
			if err != nil {
				return err
			}
			apps, err := s.api.ListApps(cmd.Context())
			if err != nil {
				return err
			}
			if len(apps) == 0 {
				return fmt.Errorf("this organization has no applications yet — create one in the Krill web UI first")
			}

			app, err := pickApp(apps, appRef)
			if err != nil {
				return err
			}
			if app.SourceType != "image" {
				fmt.Fprintf(os.Stderr,
					"warning: %s is a %s app — Krill builds it from git, so `krill-cli deploy` will refuse it.\n"+
						"Change its source to an image in the Krill UI to deploy locally built images.\n",
					app.Path, app.SourceType)
			}
			repo := app.Image
			if repo == "" {
				repo = "ghcr.io/CHANGE-ME/" + app.Name
				fmt.Fprintf(os.Stderr, "warning: %s has no image repository set in Krill; edit image.repository in %s and set the same value in the Krill UI.\n", app.Path, dest)
			}

			body := renderConfig(app.Path, repo)
			if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
				return err
			}
			fmt.Printf("Wrote %s for %s.\n\nNext: `krill-cli deploy`\n", dest, app.Path)
			return nil
		},
	}
	cmd.Flags().StringVarP(&appRef, "app", "a", "", "application path or id (default: choose interactively)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing krill.yaml")
	return cmd
}

func pickApp(apps []client.App, ref string) (client.App, error) {
	if ref != "" {
		for _, a := range apps {
			if a.Path == ref || strconv.FormatInt(a.ID, 10) == ref {
				return a, nil
			}
		}
		return client.App{}, fmt.Errorf("no application %q in this organization", ref)
	}
	if !isTerminal(os.Stdin) {
		return client.App{}, fmt.Errorf("stdin is not a terminal — pass --app project/environment/app")
	}
	fmt.Println("Which application does this project deploy to?")
	for i, a := range apps {
		fmt.Printf("  %2d) %-38s %s\n", i+1, a.Path, a.SourceType)
	}
	fmt.Print("Number: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return client.App{}, fmt.Errorf("nothing chosen")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(apps) {
		return client.App{}, fmt.Errorf("expected a number between 1 and %d", len(apps))
	}
	return apps[n-1], nil
}

// renderConfig writes the file with its explanations intact. The comments are
// the only place a user learns that the two delivery modes have very
// different costs, and that the platform default is about the SERVER.
func renderConfig(appPath, repo string) string {
	return fmt.Sprintf(`# Krill project file — commit this. It contains NO secrets.
# The server URL and API token live in ~/.config/krill/config.json
# (see: krill-cli login).

app: %s

image:
  # Must match the app's Image in Krill. krill-cli checks this before it
  # builds, because pushing to the wrong repository produces a green deploy
  # of the old image.
  repository: %s

  # The platform the SERVER runs, not the one you build on. On an Apple
  # silicon Mac this forces a cross-build; without it you push an arm64 image
  # that an amd64 VPS cannot start, and the only symptom is a container that
  # exits immediately.
  platform: %s

build:
  context: .
  dockerfile: Dockerfile
  # args: {}          # --build-arg values. NO SECRETS: this file is committed.

tag:
  strategy: git       # git | timestamp
  prefix: ""
  require_clean: false

# How the image reaches the server.
#
#   registry  (default) docker push, then Krill pulls. Only the layers that
#             changed cross the network — usually a few MB per deploy.
#
#   upload    the image is streamed to Krill, which loads it locally. No
#             registry account needed, but EVERY deploy ships the whole image
#             (hundreds of MB), not just what changed, and the server must
#             have KRILL_IMAGE_UPLOAD_ENABLED=true.
delivery: registry

# context: prod       # pin this project to one login context
`, appPath, repo, project.DefaultPlatform)
}
