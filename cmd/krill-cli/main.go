// Command krill-cli builds a container image locally and deploys it to Krill.
//
// It exists because building in CI is not always available — free build
// minutes run out — while a developer's laptop can always build. The image
// goes to a registry (or, opt-in, straight to the server) and Krill is asked
// to run it.
package main

import (
	"os"

	"github.com/proshik/krill/internal/buildinfo"
	"github.com/proshik/krill/internal/krillcli/cli"
)

func main() {
	os.Exit(cli.Execute(buildinfo.String()))
}
