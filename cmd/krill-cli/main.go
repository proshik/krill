// Command krill-cli builds a container image locally and deploys it to Krill.
//
// It exists because building in CI is not always available — free build
// minutes run out — while a developer's laptop can always build. The image
// goes to a registry (or, opt-in, straight to the server) and Krill is asked
// to run it.
package main

import (
	"os"

	"github.com/proshik/krill/internal/krillcli/cli"
)

// version is overwritten at release time with -ldflags "-X main.version=...".
// Without it the CLI falls back to the module's build info, so `go install`
// still reports something useful.
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
