package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build metadata, overridable at link time:
//
//	go build -ldflags "-X github.com/jint/searke/internal/cli.Version=v0.1.0 \
//	  -X github.com/jint/searke/internal/cli.GitCommit=$(git rev-parse --short HEAD)"
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print searke version and build metadata",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "searke %s (commit %s, built %s, %s %s/%s)\n",
				Version, GitCommit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
}
