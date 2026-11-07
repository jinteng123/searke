package cli

import (
	"fmt"
	"os"

	"github.com/jint/searke/examples"
	"github.com/spf13/cobra"
)

// newGenCmd emits annotated templates, like `sealos gen`.
func newGenCmd() *cobra.Command {
	var out string
	gen := &cobra.Command{
		Use:   "gen <cluster|bundle>",
		Short: "Print an annotated cluster.yaml or bundle.yaml template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var body string
			switch args[0] {
			case "cluster":
				body = examples.Cluster
			case "bundle":
				body = examples.Bundle
			default:
				return fmt.Errorf("unknown template %q (want cluster|bundle)", args[0])
			}
			if out == "" {
				fmt.Fprint(cmd.OutOrStdout(), body)
				return nil
			}
			if _, err := os.Stat(out); err == nil {
				return fmt.Errorf("%s already exists", out)
			}
			return os.WriteFile(out, []byte(body), 0o644)
		},
	}
	gen.Flags().StringVarP(&out, "output", "o", "", "write to file instead of stdout (refuses to overwrite)")
	return gen
}
