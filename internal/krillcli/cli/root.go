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
	"strings"

	"github.com/proshik/krill/internal/krillcli/cliconfig"
	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
	"github.com/proshik/krill/internal/krillcli/project"
	"github.com/spf13/cobra"
)

// version is the value Execute was given, which userAgent and the `version`
// command report. Resolving it (linker value, module build info, or the
// "dev" fallback) is internal/buildinfo's job, done once by the caller
// (cmd/krill-cli/main.go) before Execute ever runs.
var version string

type globals struct {
	contextName string
	server      string
	asJSON      bool
}

var g globals

// Execute runs the command tree and returns the process exit code.
func Execute(v string) int {
	version = v
	root := newRoot()
	if err := root.Execute(); err != nil {
		// Cobra has already printed usage errors; everything else is ours.
		fmt.Fprintln(os.Stderr, "Error:", classifyHint(err))
		return exitCodeFor(classify(err))
	}
	return 0
}

// classify gives every error an exit code, not just the ones the deploy flow
// produced.
//
// The documented contract (2 configuration, 3 in flight, 6 authentication) used
// to hold only inside `deploy`: a bad flag, a malformed argument, or a revoked
// token on `status` all fell through as 1 — which the same contract defines as
// "the deployment failed". A caller acting on that reads a broken pipeline as
// broken code.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var f *deployflow.Failure
	if errors.As(err, &f) {
		return err
	}
	var ae *client.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.IsUnauthorized(), ae.IsForbidden():
			return &deployflow.Failure{Code: deployflow.ExitAuth, Err: err}
		case ae.IsConflict():
			return &deployflow.Failure{Code: deployflow.ExitBusy, Err: err}
		case ae.HTTPStatus >= 400 && ae.HTTPStatus < 500:
			// Includes 404: the app, deployment or route named does not
			// exist, which is something to fix in the invocation.
			return &deployflow.Failure{Code: deployflow.ExitUsage, Err: err}
		}
	}
	return err
}

// classifyHint adds the explanation for the one API answer that is routinely
// misread. A 404 from the ROUTER (rather than from a handler) means the
// request never reached the agent API — the URL is wrong, or the server has
// KRILL_AGENT_API_ENABLED=false — but it reads as "your token was rejected",
// and people regenerate tokens for hours over it.
func classifyHint(err error) error {
	var ae *client.APIError
	if !errors.As(err, &ae) || !ae.IsNotFound() {
		return err
	}
	if !strings.Contains(strings.ToLower(ae.Message), "page not found") {
		return err
	}
	return fmt.Errorf("%w\n\n  → this 404 came from the server's router, not from Krill's API: the agent API is\n"+
		"    switched off there (KRILL_AGENT_API_ENABLED=false), or --server points at the wrong\n"+
		"    base URL. A new token will not help", err)
}

// usageArgs marks an argument-count violation as a usage error, so a mistyped
// invocation exits 2 like every other configuration problem instead of 1.
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error { return usageErr(v(c, args)) }
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

  upload               PLANNED, not implemented yet. The image would be
                       streamed straight to Krill with no registry needed, at
                       the cost of shipping the whole image on every deploy
                       rather than only the changed layers. Setting it today
                       is refused before anything is built.

Applications, projects and environments are created in the Krill web UI;
this tool only deploys ones that already exist.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// An unknown or malformed flag is a configuration problem, not a failed
	// deployment.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr(err) })
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
		newRebuildCmd(),
		newVersionCmd(),
	)
	return root
}

// userAgent identifies this client to the server, which gives an operator a
// free view of which CLI versions are in use.
func userAgent() string {
	return fmt.Sprintf("krill-cli/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
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
		c, lerr := project.Load(path)
		switch {
		case lerr == nil:
			proj = c
		case needProject:
			return nil, usageErr(lerr)
		default:
			// Reported, not fatal. Failing here would make a broken
			// krill.yaml block every command in the tree — including `init
			// --force`, whose entire job is to rewrite that file, leaving
			// hand-deletion as the only way out. Commands that do not need
			// the file carry on without it.
			fmt.Fprintf(os.Stderr, "warning: %v\n", lerr)
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
