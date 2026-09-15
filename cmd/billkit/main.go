// Command billkit is the BillKit command-line interface.
//
// The entry point sits at cmd/billkit rather than the module root because Go
// names an installed binary after the last element of the main package's import
// path. At the root that element is `billkit-cli`, the module name, and the
// module name has to be the repository URL. Here it is `billkit`, so
// `go install github.com/billkit-eu/billkit-cli/cmd/billkit@latest` produces a
// binary called `billkit`, the same name the Homebrew cask and install.sh
// deliver. The command tree itself lives in internal/cli.
package main

import (
	"fmt"
	"os"

	"github.com/billkit-eu/billkit-cli/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error: "+err.Error())
		os.Exit(1)
	}
}
