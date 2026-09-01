// Package cli wires the godwit command tree.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/version"
)

// Main runs the godwit CLI and returns the process exit code.
func Main(args []string, out, errOut io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(errOut, "godwit: %v\n", err)
		return 1
	}

	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "godwit",
		Short:         "Crash-safe database migration service",
		Long:          "godwit is a pipeline-native database migration service with crash-safe execution.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.AddCommand(newVersionCmd(), newPlanCmd(), newRunCmd(), newStatusCmd(), newDownCmd(), newServeCmd())

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the godwit version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}
