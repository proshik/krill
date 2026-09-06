// Package cli is the cobra layer: flag parsing, output formatting, and
// nothing else.
//
// Every command here parses its flags into a plain options struct and calls
// one function in a package that knows nothing about cobra. That keeps the
// dependency removable and, more usefully, keeps the logic testable without
// driving a command tree.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/proshik/krill/internal/krillcli/cliconfig"
	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
	"github.com/proshik/krill/internal/krillcli/project"
	"github.com/spf13/cobra"
)

// version is set by the linker for a release build; the fallback below covers
// `go install` and a plain `go build` from a checkout.
var version = "dev"

type globals struct {
	contextName string
	server      string
	asJSON      bool
}

var g globals

// Execute runs the command tree and returns the process exit code.
func Execute(v string) int {
	if v != "" {
		version = v
	}
	root := newRoot()
	if err := root.Execute(); err != nil {
		// Cobra has already printed usage errors; everything else is ours.
		fmt.Fprintln(os.Stderr, "Error:", err)
		return exitCodeFor(err)
	}
	return 0
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "krill-cli",
		Short: "Build locally and deploy to Krill",
		Long: `krill-cli builds a container image on this machine and deploys it to Krill.

The image reaches the server one of two ways, chosen by "delivery" in krill.yaml:

  registry  (default)  docker push, then Krill pulls. Only the layers that
                       changed cross the network, so this is much faster on
                       every deploy after the first. Needs a registry account
                       (Docker Hub, GHCR, or your own).

  upload               the image is streamed straight to Krill, which loads it
                       locally. No registry needed, but EVERY deploy ships the
                       whole image rather than the changed layers, and the
                       server must have KRILL_IMAGE_UPLOAD_ENABLED=true.

Applications, projects and environments are created in the Krill web UI;
this tool only deploys ones that already exist.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&g.contextName, "context", "c", "", "login context to use (see `krill-cli context`)")
	root.PersistentFlags().StringVar(&g.server, "server", "", "override the context's server URL")
	root.PersistentFlags().BoolVar(&g.asJSON, "json", false, "print machine-readable JSON where supported")

	root.AddCommand(
		newDeployCmd(),
		newInitCmd(),
		newLoginCmd(),
		newContextCmd(),
		newAppsCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newDeploymentsCmd(),
		newDeploymentCmd(),
		newEnvCmd(),
		newStopCmd(),
		newReloadCmd(),
		newVersionCmd(),
	)
	return root
}

// userAgent identifies this client to the server, which gives an operator a
// free view of which CLI versions are in use.
func userAgent() string {
	return fmt.Sprintf("krill-cli/%s (%s/%s)", buildVersion(), runtime.GOOS, runtime.GOARCH)
}

// buildVersion falls back to the module's build info, so `go install
// ...@v0.1.0` reports its version with no linker flags at all, and a local
// build reports its commit.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		return "dev+" + rev + dirty
	}
	return version
}

// usageErr marks an error as a configuration or argument problem, which
// exits 2. CI needs that separate from exit 1 ("the deployment failed"):
// a bad token is a pipeline to fix, a failed build is code to fix.
func usageErr(err error) error {
	if err == nil {
		return nil
	}
	var f *deployflow.Failure
	if errors.As(err, &f) {
		return err
	}
	return &deployflow.Failure{Code: deployflow.ExitUsage, Err: err}
}

// session is everything a command needs to talk to one Krill server.
type session struct {
	api      *client.Client
	resolved cliconfig.Resolved
	proj     *project.Config // nil when the command ran outside a project
}

// connect resolves the context (and the project file, if there is one) and
// builds a client.
func connect(needProject bool) (*session, error) {
	store, err := cliconfig.Load()
	if err != nil {
		return nil, usageErr(err)
	}
	if insecure, path, err := cliconfig.InsecurePermissions(); err == nil && insecure {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by other users; it holds an API token. chmod 600 it.\n", path)
	}

	var proj *project.Config
	if path, err := project.Find("."); err == nil {
		if c, lerr := project.Load(path); lerr != nil {
			// A malformed project file is worth reporting even for commands
			// that could have run without it — it is almost certainly the
			// thing the user is about to hit.
			return nil, usageErr(lerr)
		} else {
			proj = c
		}
	} else if needProject {
		return nil, usageErr(fmt.Errorf("no %s here or in any parent directory — run `krill-cli init` in your project", project.FileName))
	}

	opts := cliconfig.Options{
		ContextFlag: g.contextName,
		ServerFlag:  g.server,
		EnvToken:    os.Getenv(cliconfig.EnvToken),
		EnvServer:   os.Getenv(cliconfig.EnvServer),
		EnvContext:  os.Getenv(cliconfig.EnvContext),
	}
	if proj != nil {
		opts.ProjectContext = proj.Context
	}
	res, err := cliconfig.Resolve(store, opts)
	if err != nil {
		return nil, usageErr(err)
	}
	api, err := client.New(res.Server, res.Token, userAgent())
	if err != nil {
		return nil, usageErr(err)
	}
	return &session{api: api, resolved: res, proj: proj}, nil
}

// appRef picks the application a command acts on: the argument if given,
// otherwise krill.yaml's.
func (s *session) appRef(args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}
	if s.proj != nil {
		return s.proj.App, nil
	}
	return "", usageErr(fmt.Errorf("no application given and no %s here — pass one as an argument, e.g. acme/production/bot", project.FileName))
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
