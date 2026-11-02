// Command qstars is a command line client for the Qdrant Stars submission page.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is set at release time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Version:       version,
		Use:           "qstars",
		Short:         "Command line client for the Qdrant Stars submission page",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newLoginCmd(), newLogoutCmd(), newListCmd(), newSubmitCmd(), newQuotaCmd(), newDeleteCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
